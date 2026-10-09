package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fakeSite(t *testing.T, verdict string) *httptest.Server {
	var polls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/cli/device", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "ABCD-2345", "verification_url": "http://x/cli/auth", "interval": 0, "expires_in": 30})
	})
	mux.HandleFunc("/api/cli/token", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&polls, 1) < 2 {
			w.WriteHeader(202)
			w.Write([]byte(`{"status":"pending"}`))
			return
		}
		w.Write([]byte(`{"token":"gnm_test"}`))
	})
	mux.HandleFunc("/api/labs/week-1/01-01-add", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(labInfo{Lab: "week-1/01-01-add", Title: "Add", Editable: []string{"add.go"}, Files: []file{
			{"go.mod", "module gonomicon/lab\n\ngo 1.22\n"}, {"add.go", "package lab\n"}, {"testdata/x.txt", "hi"},
		}})
	})
	mux.HandleFunc("/api/labs/week-1/evil", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(labInfo{Lab: "week-1/evil", Files: []file{{"../../pwn", "x"}}})
	})
	mux.HandleFunc("/api/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gnm_test" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"Not logged in. Run: gonomicon login"}`))
			return
		}
		var b struct {
			Lab   string
			Files map[string]string
		}
		json.NewDecoder(r.Body).Decode(&b)
		if b.Lab != "week-1/01-01-add" || b.Files["add.go"] != "package lab\n// mine\n" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"Bad request"}`))
			return
		}
		w.Write([]byte(`{"id":"11111111-1111-1111-1111-111111111111"}`))
	})
	mux.HandleFunc("/api/submissions/", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&polls, 1)%2 == 1 {
			w.Write([]byte(`{"status":"running"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": verdict, "output": "detail line"})
	})
	return httptest.NewServer(mux)
}

func setup(t *testing.T, verdict string) {
	srv := fakeSite(t, verdict)
	t.Cleanup(srv.Close)
	site = srv.URL
	pollEvery = 5 * time.Millisecond
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	os.Chdir(t.TempDir())
}

func TestFullFlowPass(t *testing.T) {
	setup(t, "passed")
	if err := run([]string{"submit"}); err == nil || !strings.Contains(err.Error(), "logged in") {
		t.Fatalf("submit before login should fail clearly, got %v", err)
	}
	if err := run([]string{"login"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(tokenPath())
	if string(b) != "gnm_test" {
		t.Fatalf("token not saved: %q", b)
	}
	if fi, _ := os.Stat(tokenPath()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file must be 0600, got %v", fi.Mode().Perm())
	}
	if err := run([]string{"fetch", "week-1/01-01-add"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"fetch", "week-1/01-01-add"}); err == nil {
		t.Fatal("second fetch must not overwrite the learner's folder")
	}
	if _, err := os.Stat("01-01-add/testdata/x.txt"); err != nil {
		t.Fatal("nested files must be written")
	}
	os.Chdir("01-01-add")
	os.WriteFile("add.go", []byte("package lab\n// mine\n"), 0o644)
	if err := run([]string{"submit"}); err != nil {
		t.Fatalf("submit: %v", err)
	}
}

func TestFailedSubmissionIsAnError(t *testing.T) {
	setup(t, "failed")
	run([]string{"login"})
	run([]string{"fetch", "week-1/01-01-add"})
	os.Chdir("01-01-add")
	os.WriteFile("add.go", []byte("package lab\n// mine\n"), 0o644)
	if err := run([]string{"submit"}); err == nil {
		t.Fatal("a failed grade must return an error (non-zero exit)")
	}
}

func TestRejectsPathTraversal(t *testing.T) {
	setup(t, "passed")
	if err := run([]string{"fetch", "week-1/evil"}); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("expected unsafe path refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join("..", "pwn")); err == nil {
		t.Fatal("file escaped the folder")
	}
}

func TestNotLoggedIn(t *testing.T) {
	setup(t, "passed")
	run([]string{"login"})
	os.Remove(tokenPath())
	run([]string{"fetch", "week-1/01-01-add"})
	os.Chdir("01-01-add")
	if err := run([]string{"submit"}); err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("got %v", err)
	}
}

func TestBadLabKey(t *testing.T) {
	setup(t, "passed")
	if err := run([]string{"fetch", "../etc/passwd"}); err == nil {
		t.Fatal("bad key accepted")
	}
}
