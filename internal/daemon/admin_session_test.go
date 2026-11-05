package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"
)

func TestManagedAdminSessionLogsInAndStops(t *testing.T) {
	const (
		preloginCSRF = "synthetic-prelogin-csrf"
		sessionToken = "synthetic-session"
		sessionCSRF  = "synthetic-session-csrf"
		password     = "synthetic-admin-password"
	)
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/status", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "termcp_prelogin", Value: preloginCSRF, Path: "/auth/"})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mode":       "managed",
			"configured": true,
			"locked":     false,
			"csrf":       preloginCSRF,
		})
	})
	mux.HandleFunc("/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-Termcp-CSRF") != preloginCSRF {
			http.Error(w, "bad login request", http.StatusForbidden)
			return
		}
		cookie, err := r.Cookie("termcp_prelogin")
		if err != nil || cookie.Value != preloginCSRF {
			http.Error(w, "bad prelogin cookie", http.StatusForbidden)
			return
		}
		var input struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Password != password {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: managedSessionCookie, Value: sessionToken, Path: "/"})
		_ = json.NewEncoder(w).Encode(map[string]any{"csrf": sessionCSRF, "locked": false})
	})
	mux.HandleFunc(apiDaemonPath, func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(managedSessionCookie)
		if err != nil || cookie.Value != sessionToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(InstanceInfo{Daemon: true, PID: 77})
	})
	mux.HandleFunc(apiDaemonStopPath, func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(managedSessionCookie)
		if err != nil || cookie.Value != sessionToken || r.Header.Get("X-Termcp-CSRF") != sessionCSRF {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		go func() {
			time.Sleep(50 * time.Millisecond)
			server.Close()
		}()
	})
	mux.HandleFunc("/api.md", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "# Termcp HTTP API")
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	host, port := hostPort(t, server.URL)

	login, err := BeginManagedLogin(context.Background(), host, port)
	if err != nil {
		t.Fatal(err)
	}
	if !login.Status().Configured || login.Status().Locked {
		t.Fatalf("unexpected managed status: %+v", login.Status())
	}
	session, err := login.Login(context.Background(), password, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := StopEndpointWithSession(context.Background(), host, port, session)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stopped || res.PID != 77 {
		t.Fatalf("stop result = %+v", res)
	}
}

func TestBeginManagedLoginRejectsNonManagedEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	host, port := hostPort(t, server.URL)
	if _, err := BeginManagedLogin(context.Background(), host, port); err != ErrManagedAuthentication {
		t.Fatalf("BeginManagedLogin error = %v, want ErrManagedAuthentication", err)
	}
}

func TestManagedAdminLoginUnlocksConfigurationVault(t *testing.T) {
	const (
		preloginCSRF   = "synthetic-locked-prelogin"
		sessionCSRF    = "synthetic-locked-session"
		adminPassword  = "synthetic-admin-password"
		configPassword = "synthetic-config-password"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/status":
			http.SetCookie(w, &http.Cookie{Name: "termcp_prelogin", Value: preloginCSRF, Path: "/auth/"})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"mode":       "managed",
				"configured": true,
				"locked":     true,
				"csrf":       preloginCSRF,
			})
		case "/auth/login":
			var input struct {
				Password       string `json:"password"`
				ConfigPassword string `json:"config_password"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil ||
				input.Password != adminPassword ||
				input.ConfigPassword != configPassword {
				http.Error(w, "bad credentials", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: managedSessionCookie, Value: "locked-session", Path: "/"})
			_ = json.NewEncoder(w).Encode(map[string]any{"csrf": sessionCSRF, "locked": false})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	host, port := hostPort(t, server.URL)

	login, err := BeginManagedLogin(context.Background(), host, port)
	if err != nil {
		t.Fatal(err)
	}
	if !login.Status().Locked {
		t.Fatal("locked vault was not reported")
	}
	if _, err = login.Login(context.Background(), adminPassword, configPassword); err != nil {
		t.Fatal(err)
	}
}

func TestManagedAdminLoginUsesExplicitHTTPS(t *testing.T) {
	const (
		preloginCSRF = "synthetic-tls-prelogin"
		sessionCSRF  = "synthetic-tls-session"
		password     = "synthetic-tls-password"
	)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/status":
			http.SetCookie(w, &http.Cookie{Name: "termcp_prelogin", Value: preloginCSRF, Path: "/auth/"})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"mode":       "managed",
				"configured": true,
				"locked":     false,
				"csrf":       preloginCSRF,
			})
		case "/auth/login":
			var input struct {
				Password string `json:"password"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Password != password {
				http.Error(w, "bad credentials", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: managedSessionCookie, Value: "tls-session", Path: "/"})
			_ = json.NewEncoder(w).Encode(map[string]any{"csrf": sessionCSRF, "locked": false})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	client.Jar = jar
	baseURL, err := ManagementURL("", 0, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	login, err := beginManagedLoginWithClient(context.Background(), baseURL, client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = login.Login(context.Background(), password, ""); err != nil {
		t.Fatal(err)
	}
}

func TestManagedAdminLoginRefusesRedirects(t *testing.T) {
	for _, sameOrigin := range []bool{true, false} {
		t.Run(fmt.Sprintf("same_origin=%t", sameOrigin), func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "redirect target", http.StatusTeapot)
			}))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				destination := target.URL + "/elsewhere"
				if sameOrigin {
					destination = "/elsewhere"
				}
				http.Redirect(w, r, destination, http.StatusTemporaryRedirect)
			}))
			defer source.Close()
			_, err := BeginManagedLoginURL(context.Background(), source.URL)
			if !errors.Is(err, ErrUnsafeManagementURL) {
				t.Fatalf("redirect error = %v", err)
			}
		})
	}
}

func TestManagedAdminLoginRefusesLoginPostRedirect(t *testing.T) {
	const preloginCSRF = "synthetic-redirect-prelogin"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/status":
			http.SetCookie(w, &http.Cookie{Name: "termcp_prelogin", Value: preloginCSRF, Path: "/auth/"})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"mode":       "managed",
				"configured": true,
				"locked":     false,
				"csrf":       preloginCSRF,
			})
		case "/auth/login":
			http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	login, err := BeginManagedLoginURL(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = login.Login(context.Background(), "synthetic-password", "")
	if !errors.Is(err, ErrUnsafeManagementURL) {
		t.Fatalf("login redirect error = %v", err)
	}
}
