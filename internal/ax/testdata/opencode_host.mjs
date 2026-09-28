import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

let dispose, next, status = 'idle', prompts = 0;
let bound;
const binding = new Promise(resolve => { bound = resolve; });
let releaseInitialBind, readyAgain;
const restored = new Promise(resolve => { readyAgain = resolve; });
const bindings = [];
const receipts = [];
const waiters = [];
process.env.AX_OPENCODE = JSON.stringify({socket:'/fixture.sock'});
globalThis.fetch = async (url, options) => {
  const path = new URL(url).pathname;
  if (path === '/next') {
    return new Promise((resolve, reject) => {
      next = wake => resolve({ok:true, status:200, json:async () => wake});
      options.signal.addEventListener('abort', () => reject(new Error('disposed')), {once:true});
    });
  }
  if (path === '/bind') {
    const value = JSON.parse(options.body);
    bindings.push(value.state);
    if (bindings.length === 1) {
      bound();
      return new Promise(resolve => { releaseInitialBind = () => resolve({ok:true,status:204}); });
    }
    if (value.state === 'ready') readyAgain();
  }
  if (path === '/receipt') {
    const receipt = JSON.parse(options.body);
    if (receipt.deferred_state) assert.equal(bindings.at(-1), receipt.deferred_state);
    receipts.push(receipt);
    waiters.shift()?.(receipt);
  }
  return {ok:true, status:204};
};
const api = {
  state:{ready:true, config:{}, session:{
    get:() => ({}), permission:() => [], question:() => [],
    status:() => status ? {type:status} : undefined, messages:() => [],
  }},
  route:{current:{name:'session',params:{sessionID:'ses_fixture'}}},
  lifecycle:{onDispose:fn => { dispose = fn; }},
  ui:{toast:() => {}},
  client:{session:{promptAsync:async () => { prompts++; return {}; }}},
};
const source = await readFile(process.argv[2], 'utf8');
const plugin = (await import('data:text/javascript;base64,' + Buffer.from(source).toString('base64'))).default;
try {
  await plugin.tui(api);
  await binding;
  for (const active of ['busy', 'retry', 'future-active-state']) {
    status = active;
    const deferred = new Promise(resolve => waiters.push(resolve));
    next({id:'wake_' + active,native:'ses_fixture',text:'fixed ID-only wake'});
    if (active === 'busy') {
      await new Promise(resolve => setImmediate(resolve));
      assert.equal(receipts.length, 0, 'wake deferral overtook an earlier bind');
      releaseInitialBind();
    }
    assert.equal((await deferred).deferred_state, 'busy');
    assert.equal(prompts, 0, 'active wake entered the user prompt API');
    // Let the adapter reach its next long-poll without adding a timer loop.
    await new Promise(resolve => setImmediate(resolve));
  }
  status = undefined; // Idle entries are absent from the ready native store.
  const accepted = new Promise(resolve => waiters.push(resolve));
  next({id:'wake_2',native:'ses_fixture',text:'fixed ID-only wake'});
  assert.equal((await accepted).error, undefined);
  assert.equal(prompts, 1);
  assert.equal(receipts.length, 4);
  await restored;
  assert.equal(bindings.at(-1), 'ready', 'busy report stranded an idle native session');
} finally {
  dispose?.();
}
