package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"runtime"

	restserver "github.com/restic/rest-server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTLSSettings(t *testing.T) {
	type expected struct {
		TLSKey  string
		TLSCert string
		Error   bool
	}
	type passed struct {
		Path    string
		TLS     bool
		TLSKey  string
		TLSCert string
	}

	var tests = []struct {
		passed   passed
		expected expected
	}{
		{passed{TLS: false}, expected{"", "", false}},
		{passed{TLS: true}, expected{
			filepath.Join(os.TempDir(), "restic/private_key"),
			filepath.Join(os.TempDir(), "restic/public_key"),
			false,
		}},
		{passed{
			Path: os.TempDir(),
			TLS:  true,
		}, expected{
			filepath.Join(os.TempDir(), "private_key"),
			filepath.Join(os.TempDir(), "public_key"),
			false,
		}},
		{passed{Path: os.TempDir(), TLS: true, TLSKey: "/etc/restic/key", TLSCert: "/etc/restic/cert"}, expected{"/etc/restic/key", "/etc/restic/cert", false}},
		{passed{Path: os.TempDir(), TLS: false, TLSKey: "/etc/restic/key", TLSCert: "/etc/restic/cert"}, expected{"", "", true}},
		{passed{Path: os.TempDir(), TLS: false, TLSKey: "/etc/restic/key"}, expected{"", "", true}},
		{passed{Path: os.TempDir(), TLS: false, TLSCert: "/etc/restic/cert"}, expected{"", "", true}},
	}

	for _, test := range tests {
		app := newRestServerApp()
		t.Run("", func(t *testing.T) {
			// defer func() { restserver.Server = defaultConfig }()
			if test.passed.Path != "" {
				app.Server.Path = test.passed.Path
			}
			app.Server.TLS = test.passed.TLS
			app.Server.TLSKey = test.passed.TLSKey
			app.Server.TLSCert = test.passed.TLSCert

			gotTLS, gotKey, gotCert, err := app.tlsSettings()
			if err != nil && !test.expected.Error {
				t.Fatalf("tls_settings returned err (%v)", err)
			}
			if test.expected.Error {
				if err == nil {
					t.Fatalf("Error not returned properly (%v)", test)
				} else {
					return
				}
			}
			if gotTLS != test.passed.TLS {
				t.Errorf("TLS enabled, want (%v), got (%v)", test.passed.TLS, gotTLS)
			}
			wantKey := test.expected.TLSKey
			if gotKey != wantKey {
				t.Errorf("wrong TLSPrivPath path, want (%v), got (%v)", wantKey, gotKey)
			}

			wantCert := test.expected.TLSCert
			if gotCert != wantCert {
				t.Errorf("wrong TLSCertPath path, want (%v), got (%v)", wantCert, gotCert)
			}

		})
	}
}


func TestGetHandler(t *testing.T) {
	dir, err := os.MkdirTemp("", "rest-server-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err := os.Remove(dir)
		if err != nil {
			t.Fatal(err)
		}
	}()

	getHandler := restserver.NewHandler

	// With NoAuth = false and no .htpasswd
	_, err = getHandler(&restserver.Server{Path: dir})
	if err == nil {
		t.Errorf("NoAuth=false: expected error, got nil")
	}

	// With NoAuth = true and no .htpasswd
	_, err = getHandler(&restserver.Server{NoAuth: true, Path: dir})
	if err != nil {
		t.Errorf("NoAuth=true: expected no error, got %v", err)
	}

	// With NoAuth = false, no .htpasswd and ProxyAuth = X-Remote-User
	_, err = getHandler(&restserver.Server{Path: dir, ProxyAuthUsername: "X-Remote-User"})
	if err != nil {
		t.Errorf("NoAuth=false, ProxyAuthUsername = X-Remote-User: expected no error, got %v", err)
	}

	// With NoAuth = false and custom .htpasswd
	htpFile, err := os.CreateTemp(dir, "custom")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err := os.Remove(htpFile.Name())
		if err != nil {
			t.Fatal(err)
		}
	}()
	_, err = getHandler(&restserver.Server{HtpasswdPath: htpFile.Name()})
	if err != nil {
		t.Errorf("NoAuth=false with custom htpasswd: expected no error, got %v", err)
	}

	// Create .htpasswd
	htpasswd := filepath.Join(dir, ".htpasswd")
	err = os.WriteFile(htpasswd, []byte(""), 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		err := os.Remove(htpasswd)
		if err != nil {
			t.Fatal(err)
		}
	}()

	// With NoAuth = false and with .htpasswd
	_, err = getHandler(&restserver.Server{Path: dir})
	if err != nil {
		t.Errorf("NoAuth=false with .htpasswd: expected no error, got %v", err)
	}
}

// helper method to test the app. Starts app with passed arguments,
// then will call the callback function which can make requests against
// the application. If the callback function fails due to errors returned
// by http.Do() (i.e. *url.Error), then it will be retried until successful,
// or the passed timeout passes.
func testServerWithArgs(args []string, timeout time.Duration, cb func(context.Context, *restServerApp) error) error {
	// create the app with passed args
	app := newRestServerApp()
	app.CmdRoot.SetArgs(args)

	// create context that will timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// wait group for our client and server tasks
	jobs := &sync.WaitGroup{}
	jobs.Add(2)

	// run the server, saving the error
	var serverErr error
	go func() {
		defer jobs.Done()
		defer cancel() // if the server is stopped, no point keep the client alive
		serverErr = app.CmdRoot.ExecuteContext(ctx)
	}()

	// run the client, saving the error
	var clientErr error
	go func() {
		defer jobs.Done()
		defer cancel() // once the client is done, stop the server

		var urlError *url.Error

		// execute in loop, as we will retry for network errors
		// (such as the server hasn't started yet)
		for {
			clientErr = cb(ctx, app)
			switch {
			case clientErr == nil:
				return // success, we're done
			case errors.As(clientErr, &urlError):
				// if a network error (url.Error), then wait and retry
				// as server may not be ready yet
				select {
				case <-time.After(time.Millisecond * 100):
					continue
				case <-ctx.Done(): // unless we run out of time first
					clientErr = context.Canceled
					return
				}
			default:
				return // other error type, we're done
			}
		}
	}()

	// wait for both to complete
	jobs.Wait()

	// report back if either failed
	if clientErr != nil || serverErr != nil {
		return fmt.Errorf("client or server error, client: %v, server: %v", clientErr, serverErr)
	}

	return nil
}


func TestHttpListen(t *testing.T) {
	td := t.TempDir()

	// create some content and parent dirs
	if err := os.MkdirAll(filepath.Join(td, "data", "repo1"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "data", "repo1", "config"), []byte("foo"), 0700); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"--no-auth", "--path", filepath.Join(td, "data"), "--listen", "127.0.0.1:0"},    // test emphemeral port
		{"--no-auth", "--path", filepath.Join(td, "data"), "--listen", "127.0.0.1:9000"}, // test "normal" port
		{"--no-auth", "--path", filepath.Join(td, "data"), "--listen", "127.0.0.1:9000"}, // test that server was shutdown cleanly and that we can re-use that port
	} {
		err := testServerWithArgs(args, time.Second*10, func(ctx context.Context, app *restServerApp) error {
			for _, test := range []struct {
				Path       string
				StatusCode int
			}{
				{"/repo1/", http.StatusMethodNotAllowed},
				{"/repo1/config", http.StatusOK},
				{"/repo2/config", http.StatusNotFound},
			} {
				listenAddr := app.ListenerAddress()
				if listenAddr == nil {
					return &url.Error{} // return this type of err, as we know this will retry
				}
				port := strings.Split(listenAddr.String(), ":")[1]

				req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://localhost:%s%s", port, test.Path), nil)
				if err != nil {
					return err
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return err
				}
				err = resp.Body.Close()
				if err != nil {
					return err
				}
				if resp.StatusCode != test.StatusCode {
					return fmt.Errorf("expected %d from server, instead got %d (path %s)", test.StatusCode, resp.StatusCode, test.Path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestNewRestServerApp_DefaultsAndArgs_001 tests default values and argument parsing.

func TestRunRoot_CPUProfiling_Success_005(t *testing.T) {
	app := newRestServerApp()
	tempFile, err := os.CreateTemp("", "cpu-profile-*.prof")
	require.NoError(t, err)
	profilePath := tempFile.Name()
	_ = tempFile.Close() // Close the file so pprof can write to it.
	defer os.Remove(profilePath)

	app.CPUProfile = profilePath
	app.Server.Listen = "127.0.0.1:0"
	app.Server.NoAuth = true

	ctx, cancel := context.WithCancel(context.Background())
	app.CmdRoot.SetContext(ctx)

	var runErr error
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		runErr = app.runRoot(app.CmdRoot, []string{})
	}()

	time.Sleep(150 * time.Millisecond) // Give time for profile to start and server to initialize
	cancel()
	wg.Wait()

	if runErr != nil && !errors.Is(runErr, http.ErrServerClosed) {
		// http.ErrServerClosed is acceptable if server started and then closed.
		// context.Canceled might also be possible depending on exact shutdown sequence.
		// For this test, the primary check is the profile file.
		t.Logf("app.runRoot returned an error: %v", runErr)
	}

	info, err := os.Stat(profilePath)
	require.NoError(t, err, "CPU profile file should exist")
	assert.Greater(t, info.Size(), int64(0), "CPU profile file should not be empty")
}

// TestRunRoot_LoggingBranches_012 exercises different logging paths in runRoot.

func TestRunRoot_LoggingBranches_012(t *testing.T) {
	testCases := []struct {
		name     string
		setupApp func(app *restServerApp)
		// We can't easily check log output without redirecting,
		// so this test focuses on path coverage.
	}{
		{
			name: "NoAuth",
			setupApp: func(app *restServerApp) {
				app.Server.NoAuth = true
			},
		},
		{
			name: "AuthEnabled (default htpasswd)",
			setupApp: func(app *restServerApp) {
				app.Server.NoAuth = false
				// Create a dummy .htpasswd so NewHandler doesn't fail early
				dummyHtpasswd := filepath.Join(app.Server.Path, ".htpasswd")
				os.WriteFile(dummyHtpasswd, []byte("testuser:$apr1$blahblah$blah"), 0600)
			},
		},
		{
			name: "ProxyAuthEnabled",
			setupApp: func(app *restServerApp) {
				app.Server.NoAuth = false
				app.Server.ProxyAuthUsername = "X-Remote-User"
			},
		},
		{
			name:     "AppendOnly",
			setupApp: func(app *restServerApp) { app.Server.AppendOnly = true },
		},
		{
			name:     "PrivateRepos",
			setupApp: func(app *restServerApp) { app.Server.PrivateRepos = true },
		},
		{
			name:     "GroupAccessibleRepos",
			setupApp: func(app *restServerApp) { app.Server.GroupAccessibleRepos = true },
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			app := newRestServerApp()
			app.Server.Path = t.TempDir()     // Ensure clean path for htpasswd if needed
			app.Server.Listen = "127.0.0.1:0" // Allow server to start
			if tc.name != "AuthEnabled (default htpasswd)" && tc.name != "ProxyAuthEnabled" {
				app.Server.NoAuth = true // Default to NoAuth for simplicity unless testing auth
			}

			if tc.setupApp != nil {
				tc.setupApp(app)
			}

			ctx, cancel := context.WithCancel(context.Background())
			app.CmdRoot.SetContext(ctx)

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Errors during startup are not the focus here, but path coverage.
				_ = app.runRoot(app.CmdRoot, []string{})
			}()

			time.Sleep(50 * time.Millisecond) // Allow time for logs to be printed
			cancel()
			wg.Wait()
			// No specific assertions on logs, just exercising the code paths.
		})
	}
}

func TestRunRoot_CPUProfileCreateError_004(t *testing.T) {
	app := newRestServerApp()
	// Path that cannot be created (e.g. a directory with the same name or read-only location)
	// For simplicity, use a path that's highly likely to be invalid for file creation.
	// Creating a directory with the same name as the file.
	cpuProfileDir := filepath.Join(t.TempDir(), "cpu-profile-dir")
	err := os.Mkdir(cpuProfileDir, 0755)
	require.NoError(t, err)
	app.CPUProfile = cpuProfileDir // Trying to create a file where a directory exists

	app.Server.Listen = "127.0.0.1:0" // Minimal config
	app.Server.NoAuth = true

	ctx, cancel := context.WithCancel(context.Background())
	app.CmdRoot.SetContext(ctx) // Set context for CmdRoot
	defer cancel()              // Cancel at the end if not already

	// Execute RunE directly
	runErr := app.runRoot(app.CmdRoot, []string{})
	require.Error(t, runErr)
	// The specific error depends on OS, but it should be a path error.
	// Example: "open <path>: is a directory" or "permission denied"
	assert.Contains(t, runErr.Error(), cpuProfileDir)
}

// TestRunRoot_CPUProfiling_Success_005 verifies CPU profiling file creation.

func TestNewRestServerApp_DefaultsAndArgs_001(t *testing.T) {
	app := newRestServerApp()

	assert.NotNil(t, app.CmdRoot)
	assert.Equal(t, "rest-server", app.CmdRoot.Use)
	assert.Equal(t, filepath.Join(os.TempDir(), "restic"), app.Server.Path)
	assert.Equal(t, ":8000", app.Server.Listen)
	assert.Equal(t, "1.2", app.Server.TLSMinVer)

	// Test Args function
	err := app.CmdRoot.Args(app.CmdRoot, []string{})
	assert.NoError(t, err)

	err = app.CmdRoot.Args(app.CmdRoot, []string{"foo"})
	assert.Error(t, err)
	assert.EqualError(t, err, "rest-server expects no arguments - unknown argument: foo")

	expectedVersion := fmt.Sprintf("rest-server %s compiled with %v on %v/%v\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	assert.Equal(t, expectedVersion, app.CmdRoot.Version)
}

// TestRunRoot_CPUProfileCreateError_004 tests runRoot when os.Create for CPU profile fails.

