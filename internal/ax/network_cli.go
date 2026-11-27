//go:build darwin || linux

package ax

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"time"
)

const defaultNetworkPort = 48889

func networkCLI(ctx context.Context, dir string, args []string, in io.Reader, out io.Writer) error {
	usage := errors.New("usage: ax network invite NAME [AGENT...] | join NAME [AGENT...] | start | init NAME [PORT] | identity | trust ALIAS FILE [AGENT...] | messaging ALIAS on|off | revoke ALIAS | serve | agents ALIAS")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "invite", "join":
		if len(args) < 2 {
			return usage
		}
		return networkPairCLI(ctx, dir, args[0], args[1], args[2:], in, out)
	case "start":
		if len(args) != 1 {
			return usage
		}
		return enableNetworkAdapter(ctx, dir)
	case "init":
		if len(args) != 2 && len(args) != 3 {
			return usage
		}
		port := uint64(defaultNetworkPort)
		var err error
		if len(args) == 3 {
			port, err = strconv.ParseUint(args[2], 10, 16)
		}
		if err != nil || port == 0 {
			return errors.New("choose a port between 1 and 65535")
		}
		card, err := initNetwork(ctx, dir, args[1], uint16(port))
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(card)
	case "identity":
		if len(args) != 1 {
			return usage
		}
		identity, _, err := loadNetworkIdentity(dir)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(identity.Card)
	case "trust":
		if len(args) < 3 {
			return usage
		}
		if _, _, err := loadNetworkIdentity(dir); err != nil {
			return err
		}
		if args[2] != "-" {
			file, err := os.Open(args[2])
			if err != nil {
				return err
			}
			defer file.Close()
			in = file
		}
		data, err := io.ReadAll(io.LimitReader(in, (16<<10)+1))
		var card networkCard
		if err != nil || len(data) > 16<<10 || json.Unmarshal(data, &card) != nil {
			return errors.New("invalid or oversized pairing card")
		}
		if err = trustNetworkPeer(dir, args[1], card, args[3:]); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Paired %s, certificate SHA256 %s. Discovery only.\n", args[1], networkFingerprint(card))
		return err
	case "messaging":
		if len(args) != 3 || (args[2] != "on" && args[2] != "off") {
			return usage
		}
		return setNetworkMessaging(dir, args[1], args[2] == "on")
	case "revoke":
		if len(args) != 2 {
			return usage
		}
		return revokeNetworkPeer(dir, args[1])
	case "agents":
		if len(args) != 2 {
			return usage
		}
		agents, err := discoverNetwork(ctx, dir, args[1])
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(agents)
	case "serve":
		if len(args) != 1 {
			return usage
		}
		identity, cert, err := loadNetworkIdentity(dir)
		if err != nil {
			return err
		}
		ips, err := tailscaleAddresses(ctx)
		if err != nil {
			return err
		}
		u, _ := url.Parse(identity.Card.Address)
		ip, _ := netip.ParseAddr(u.Hostname())
		found := false
		for _, local := range ips {
			found = found || ip == local
		}
		if !found {
			return errors.New("saved network address is not assigned to this Tailscale device")
		}
		listener, err := net.Listen("tcp", u.Host)
		if err != nil {
			return err
		}
		defer listener.Close()
		lock, err := networkRunLock(dir)
		if err != nil {
			return err
		}
		defer lock.Close()
		ready, err := listenPrivateSocket(networkReadySocket(dir))
		if err != nil {
			return err
		}
		defer ready.Close()
		// Reserve the adapter before starting a broker that might otherwise
		// try to restore it in the background.
		if err = ensureBroker(dir); err != nil {
			return err
		}
		go acceptNetworkReadiness(ready)
		child, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); runNetworkWorkers(child, dir, cert) }()
		err = serveNetwork(child, dir, listener, identity, cert)
		cancel()
		<-done
		return err
	default:
		return usage
	}
}

func networkPairCLI(ctx context.Context, dir, action, name string, exports []string, in io.Reader, out io.Writer) error {
	if err := validateNetworkExports(name, exports); err != nil {
		return err
	}
	var invite networkInvite
	var err error
	if action == "join" {
		fmt.Fprintln(out, "Paste the AX invitation and press Enter.")
		invite, err = awaitNetworkInvite(ctx, in)
		if err != nil {
			return err
		}
		if invite.Host.Name == name {
			return errors.New("choose a machine name different from the inviter")
		}
	}
	identity, cert, err := loadNetworkIdentity(dir)
	if os.IsNotExist(err) {
		if _, err := initNetwork(ctx, dir, name, defaultNetworkPort); err != nil {
			return err
		}
		identity, cert, err = loadNetworkIdentity(dir)
	}
	if err != nil {
		return err
	}
	if identity.Card.Name != name {
		return fmt.Errorf("this AX installation is already named %s", identity.Card.Name)
	}
	if action == "invite" {
		if err := enableNetworkAdapter(ctx, dir); err != nil {
			return err
		}
		token, err := createNetworkInvite(dir, identity.Card, exports, time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "Paste this single-use invitation into ax network join on the other machine. It expires in 10 minutes.")
		_, err = fmt.Fprintln(out, token)
		return err
	}
	client, err := networkHTTPClient(cert, invite.Host)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if err := joinNetwork(ctx, dir, invite, identity.Card, exports, client); err != nil {
		return err
	}
	if err := enableNetworkAdapter(ctx, dir); err != nil {
		return fmt.Errorf("paired with %s; retry startup with ax network start: %w", invite.Host.Name, err)
	}
	_, err = fmt.Fprintf(out, "Paired with %s. Shared agents will appear in ax agents as NAME@%s.\n", invite.Host.Name, invite.Host.Name)
	return err
}
