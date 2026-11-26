// Package tunnelgateway is the public tunnel server: it serves `<slug>.<domain>`
// by forwarding each request to the host that opened the tunnel, and
// `h-<host id>.<domain>` to that host's plugins, over the socket each host keeps
// open at `/connect`. TLS ends before it, so it reads every request in the clear.
package tunnelgateway
