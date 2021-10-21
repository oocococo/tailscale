package main

import (
	"bytes"
	"fmt"
	"html"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall/js"

	"tailscale.com/cmd/tailscale/cli"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/net/netns"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/netstack"
)

func main() {
	conf := wgengine.Config{
		RespondToPing: true,
	}
	netns.SetEnabled(false)
	var logf logger.Logf = log.Printf
	eng, err := wgengine.NewUserspaceEngine(logf, conf)
	if err != nil {
		log.Fatal(err)
	}
	tunDev, magicConn, ok := eng.(wgengine.InternalsGetter).GetInternals()
	if !ok {
		log.Fatalf("%T is not a wgengine.InternalsGetter", eng)
	}
	onlySubnets := false
	ns, err := netstack.Create(logf, tunDev, eng, magicConn, onlySubnets)
	if err != nil {
		log.Fatalf("netstack.Create: %v", err)
	}
	if err := ns.Start(); err != nil {
		log.Fatalf("failed to start netstack: %v", err)
	}

	lb, err := ipnlocal.NewLocalBackend(log.Printf, "some-logid", new(ipn.MemoryStore), eng)
	if err != nil {
		log.Fatal(err)
	}

	doc := js.Global().Get("document")
	state := doc.Call("getElementById", "state")
	netmapEle := doc.Call("getElementById", "netmap")
	loginEle := doc.Call("getElementById", "loginURL")

	state.Set("innerHTML", "ready")

	lb.SetNotifyCallback(func(n ipn.Notify) {
		log.Printf("NOTIFY: %+v", n)
		if n.State != nil {
			state.Set("innerHTML", fmt.Sprint(*n.State))
			switch *n.State {
			case ipn.Running, ipn.Starting:
				loginEle.Set("innerHTML", "")
			}
		}
		if nm := n.NetMap; nm != nil {
			var buf bytes.Buffer
			fmt.Fprintf(&buf, "<p>Name: <b>%s</b></p>\n", html.EscapeString(nm.Name))
			fmt.Fprintf(&buf, "<p>Addresses: ")
			for i, a := range nm.Addresses {
				if i == 0 {
					fmt.Fprintf(&buf, "<b>%s</b>", a.IP())
				} else {
					fmt.Fprintf(&buf, ", %s", a.IP())
				}
			}
			fmt.Fprintf(&buf, "</p>")
			fmt.Fprintf(&buf, "<p>Machine: <b>%v</b>, %v</p>\n", nm.MachineStatus, nm.MachineKey)
			fmt.Fprintf(&buf, "<p>Nodekey: %v</p>\n", nm.NodeKey)
			fmt.Fprintf(&buf, "<hr><table>")
			for _, p := range nm.Peers {
				var ip string
				if len(p.Addresses) > 0 {
					ip = p.Addresses[0].IP().String()
				}
				fmt.Fprintf(&buf, "<tr><td>%s</td><td>%s</td></tr>\n", ip, html.EscapeString(p.Name))
			}
			fmt.Fprintf(&buf, "</table>")
			netmapEle.Set("innerHTML", buf.String())
		}
		if n.BrowseToURL != nil {
			esc := html.EscapeString(*n.BrowseToURL)
			loginEle.Set("innerHTML", fmt.Sprintf("<a href='%s' target=_blank>%s</a>", esc, esc))
		}
	})

	start := func() {
		err := lb.Start(ipn.Options{
			Prefs: &ipn.Prefs{
				// go run ./cmd/trunkd/  -remote-url=https://controlplane.tailscale.com
				//ControlURL:       "http://tsdev:8080",
				ControlURL:       "https://controlplane.tailscale.com",
				RouteAll:         false,
				AllowSingleHosts: true,
				WantRunning:      true,
				Hostname:         "wasm",
			},
		})
		log.Printf("Start error: %v", err)

	}

	js.Global().Set("startClicked", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		go start()
		return nil
	}))

	js.Global().Set("logoutClicked", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		log.Printf("Logout clicked")
		if lb.State() == ipn.NoState {
			log.Printf("Backend not running")
			return nil
		}
		go lb.Logout()
		return nil
	}))

	js.Global().Set("startLoginInteractive", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		log.Printf("State: %v", lb.State)

		go func() {
			if lb.State() == ipn.NoState {
				start()
			}
			lb.StartLoginInteractive()
		}()
		return nil
	}))

	js.Global().Set("seeGoroutines", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		full := make([]byte, 1<<20)
		buf := full[:runtime.Stack(full, true)]
		js.Global().Get("theTerminal").Call("reset")
		withCR := make([]byte, 0, len(buf)+bytes.Count(buf, []byte{'\n'}))
		for _, b := range buf {
			if b == '\n' {
				withCR = append(withCR, "\r\n"...)
			} else {
				withCR = append(withCR, b)
			}
		}
		js.Global().Get("theTerminal").Call("write", string(withCR))
		return nil
	}))

	js.Global().Set("startAuthKey", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		authKey := args[0].String()
		log.Printf("got auth key")
		go func() {
			err := lb.Start(ipn.Options{
				Prefs: &ipn.Prefs{
					// go run ./cmd/trunkd/  -remote-url=https://controlplane.tailscale.com
					//ControlURL:       "http://tsdev:8080",
					ControlURL:       "https://controlplane.tailscale.com",
					RouteAll:         false,
					AllowSingleHosts: true,
					WantRunning:      true,
					Hostname:         "wasm",
				},
				AuthKey: authKey,
			})
			log.Printf("Start error: %v", err)
		}()
		return nil
	}))

	var termOutOnce sync.Once

	js.Global().Set("runTailscaleCLI", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		if len(args) < 1 {
			log.Printf("missing args")
			return nil
		}
		// TODO(bradfitz): enforce that we're only running one
		// CLI command at a time, as we modify package cli
		// globals below, like cli.Fatalf.

		go func() {
			if len(args) >= 2 {
				onDone := args[1]
				defer onDone.Invoke() // re-print the prompt
			}
			/*
				fs := js.Global().Get("globalThis").Get("fs")
				oldWriteSync := fs.Get("writeSync")
				defer fs.Set("writeSync", oldWriteSync)

				fs.Set("writeSync", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
					if len(args) != 2 {
						return nil
					}
					js.Global().Get("theTerminal").Call("write", fmt.Sprintf("Got a %T %v\r\n", args[1], args[1]))
					return nil
				}))
			*/
			line := args[0].String()
			f := strings.Fields(line)
			term := js.Global().Get("theTerminal")
			termOutOnce.Do(func() {
				cli.Stdout = termWriter{term}
				cli.Stderr = termWriter{term}
			})

			cli.Fatalf = func(format string, a ...interface{}) {
				term.Call("write", strings.ReplaceAll(fmt.Sprintf(format, a...), "\n", "\n\r"))
				runtime.Goexit()
			}

			// TODO(bradfitz): add a cli package global logger and make that
			// package use it, rather than messing with log.SetOutput.
			log.SetOutput(cli.Stderr)
			defer log.SetOutput(os.Stderr) // back to console

			defer func() {
				if e := recover(); e != nil {
					term.Call("write", fmt.Sprintf("%s\r\n", e))
					fmt.Fprintf(os.Stderr, "recovered panic from %q: %v", f, e)
				}
			}()

			if err := cli.Run(f[1:]); err != nil {
				fmt.Fprintf(os.Stderr, "CLI error on %q: %v\n", f, err)
				term.Call("write", fmt.Sprintf("%v\r\n", err))
				return
			}
		}()
		return nil
	}))

	<-make(chan bool)
}

type termWriter struct {
	o js.Value
}

func (w termWriter) Write(p []byte) (n int, err error) {
	r := bytes.Replace(p, []byte("\n"), []byte("\n\r"), -1)
	w.o.Call("write", string(r))
	return len(p), nil
}
