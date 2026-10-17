import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';
import { readFile } from 'node:fs/promises';
import { Type } from 'typebox';

// Pi owns its terminal. This extension owns only an optional messaging child.
export default function ax(pi) {
  const options = JSON.parse(process.env.AX_PI);
  let current, connection, retry, connecting, sequence = 0, retryDelay = 1000;
  const isCurrent = session => current === session && session.ctx.sessionManager.getSessionId() === session.id;
  const live = c => c && connection === c && isCurrent(c.session);
  const status = text => current?.ctx.ui.setStatus('ax', text);

  function disconnect(c, error) {
    if (!c) return;
    if (connection === c) connection = undefined;
    c.lines.close();
    c.child.kill();
    for (const request of c.pending.values()) request.reject(error);
    c.pending.clear();
  }

  function write(c, packet, callback) {
    if (!live(c)) throw new Error('AX conversation changed or messaging disconnected');
    c.child.stdin.write(JSON.stringify({ jsonrpc: '2.0', ...packet }) + '\n', callback);
  }

  function rpc(c, method, params) {
    if (!live(c)) return Promise.reject(new Error('AX messaging is reconnecting'));
    if (c.pending.size >= 32) return Promise.reject(new Error('AX messaging is busy; retry shortly'));
    const id = ++sequence;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => finish(new Error('AX messaging timed out')), 15000);
      function finish(error, value) {
        clearTimeout(timer);
        c.pending.delete(id);
        error ? reject(error) : resolve(value);
      }
      c.pending.set(id, { resolve: value => finish(null, value), reject: finish });
      try { write(c, { id, method, params }, error => { if (error) finish(error); }); }
      catch (error) { finish(error); }
    });
  }

  async function tool(c, name, args) {
    const result = await rpc(c, 'tools/call', { name, arguments: args });
    if (!live(c)) throw new Error('AX conversation changed during the tool call');
    if (result.isError) throw new Error(result.content?.[0]?.text || 'AX tool failed');
    return { content: result.content, details: {} };
  }

  function lifecycle(session, event) {
    // Serialize state reports, including reconnects, so a late busy report cannot
    // overwrite a subsequent idle report. Never start a hook for an old session.
    const next = session.hooks.then(() => {
      if (!isCurrent(session)) return;
      return new Promise((resolve, reject) => {
        const hook = spawn(options.command, ['hook'], { stdio: ['pipe', 'ignore', 'pipe'] });
        let message = '';
        const timeout = setTimeout(() => hook.kill('SIGKILL'), 3000);
        hook.stderr.on('data', data => { message = (message + data).slice(-2048); });
        hook.stdin.on('error', () => {});
        hook.on('error', error => { clearTimeout(timeout); reject(error); });
        hook.on('close', code => {
          clearTimeout(timeout);
          code === 0 ? resolve() : reject(new Error(message.trim() || 'AX lifecycle unavailable'));
        });
        hook.stdin.end(JSON.stringify({ session_id: session.id, session_file: session.file,
          hook_event_name: event, permission_mode: 'native' }));
      });
    });
    session.hooks = next.catch(() => {});
    return next;
  }

  function schedule(session) {
    if (!isCurrent(session) || retry) return;
    retry = setTimeout(() => {
      retry = undefined;
      if (connecting) schedule(session);
      else void connect(session);
    }, retryDelay);
    retryDelay = Math.min(retryDelay * 2, 15000);
  }

  async function connect(session) {
    if (!isCurrent(session) || connection || connecting) return;
    const attempt = {};
    connecting = attempt;
    let c;
    try {
      const saved = JSON.parse(await readFile(options.sessionFile, 'utf8'));
      if (!isCurrent(session)) return;
      if (saved.native_session_id && saved.native_session_id !== session.id && (saved.started || !saved.launch_id)) {
        status('AX name belongs to another conversation; launch with a new AX name');
        return;
      }
      await lifecycle(session, 'SessionStart');
      if (!isCurrent(session)) return;
      if (!session.ctx.isIdle()) await lifecycle(session, 'UserPromptSubmit');
      if (!isCurrent(session)) return;
      const child = spawn(options.command, ['bridge'], { stdio: ['pipe', 'pipe', 'ignore'] });
      c = { child, session, pending: new Map(), lines: createInterface({ input: child.stdout }) };
      connection = c;
      const failed = error => {
        if (!live(c)) return;
        if (c.readyAt && Date.now() - c.readyAt > 30000) retryDelay = 1000;
        disconnect(c, error);
        status('AX reconnecting');
        schedule(session);
      };
      child.on('error', failed);
      child.on('close', () => failed(new Error('AX messaging disconnected')));
      child.stdin.on('error', failed);
      c.lines.on('line', line => {
        if (!live(c)) return;
        try {
          const message = JSON.parse(line);
          if (message.id !== undefined) {
            const request = c.pending.get(message.id);
            if (message.error) request?.reject(new Error(message.error.message));
            else request?.resolve(message.result);
          } else if (message.method === 'notifications/ax/message') {
            const p = message.params;
            if (p.native_session_id !== session.id || typeof p.content !== 'string') return;
            pi.sendMessage({ customType: 'ax', content: p.content, display: true, details: p.meta },
              { triggerTurn: true, deliverAs: 'followUp' });
          }
        } catch (error) { failed(error); }
      });
      await rpc(c, 'initialize', { protocolVersion: '2025-11-25', capabilities: {}, clientInfo: { name: 'ax-pi', version: '1' } });
      write(c, { method: 'notifications/initialized' });
      await rpc(c, 'tools/list', {});
      await tool(c, 'list_agents', {}); // Connect without a model setup turn.
      if (live(c)) {
        c.readyAt = Date.now();
        status('AX connected');
      }
    } catch (error) {
      disconnect(c, error);
      if (isCurrent(session)) {
        status('AX unavailable: ' + error.message);
        schedule(session);
      }
    } finally {
      if (connecting === attempt) connecting = undefined;
    }
  }

  for (const spec of options.tools) {
    const fields = Object.fromEntries(Object.entries(spec.inputSchema.properties).map(([name, value]) => {
      const schema = value.type === 'array'
        ? Type.Array(Type.String(), { maxItems: value.maxItems, description: value.description })
        : value.type === 'integer'
          ? Type.Integer({ minimum: value.minimum, maximum: value.maximum, description: value.description })
          : Type.String({ description: value.description });
      return [name, spec.inputSchema.required.includes(name) ? schema : Type.Optional(schema)];
    }));
    pi.registerTool({
      name: 'ax_' + spec.name, label: 'AX ' + spec.name, description: spec.description,
      parameters: Type.Object(fields, { additionalProperties: false }),
      async execute(_id, args) {
        if (!live(connection) || !connection.readyAt) throw new Error('AX messaging is reconnecting; retry shortly');
        return tool(connection, spec.name, args);
      },
    });
  }

  function stop() {
    current = undefined;
    clearTimeout(retry);
    retry = undefined;
    connecting = undefined;
    disconnect(connection, new Error('AX session closed'));
  }
  // Pi emits session_start for startup, reload, new, resume, and fork.
  pi.on('session_start', (_event, ctx) => {
    stop();
    retryDelay = 1000;
    current = { ctx, id: ctx.sessionManager.getSessionId(), file: ctx.sessionManager.getSessionFile(), hooks: Promise.resolve() };
    void connect(current);
  });
  pi.on('session_shutdown', stop);
  pi.on('before_agent_start', event => ({ systemPrompt: event.systemPrompt + '\n\n' + options.instructions }));
  for (const [event, hook] of [['agent_start', 'UserPromptSubmit'], ['agent_end', 'Stop']]) {
    pi.on(event, () => {
      const session = current;
      if (session && isCurrent(session)) {
        return lifecycle(session, hook).catch(error => {
          if (isCurrent(session)) status('AX unavailable: ' + error.message);
        });
      }
    });
  }
}
