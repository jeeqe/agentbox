package server

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// guard protects the dashboard from other web pages. The server only listens on
// loopback, but a page open in the same browser can still reach it, so two
// things are checked:
//
//   - The Host header must be a loopback name. A DNS-rebinding page resolves its
//     own hostname to 127.0.0.1 and would otherwise be able to read everything.
//   - A request that changes state must come from the dashboard itself. Browsers
//     always attach Origin (and Sec-Fetch-Site) to cross-site POSTs, so a
//     mismatch is refused. Requests with neither header (curl, scripts, tests)
//     are allowed: they are not a browser acting for some other site.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden: unexpected Host header", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				http.Error(w, "forbidden: cross-site request", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
