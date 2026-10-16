// gonomicon: fetch a lab, test it locally, and submit it for grading. Standard library only.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var version = "dev"

const marker = ".gonomicon"

var (
	site       = envOr("GONOMICON_SITE", "https://gonomicon.com")
	httpClient = &http.Client{Timeout: 30 * time.Second}
	pollEvery  = 3 * time.Second
	pollFor    = 4 * time.Minute
	labKeyRE   = regexp.MustCompile(`^[\w-]+/[\w-]+$`)
	safePathRE = regexp.MustCompile(`^\.?[A-Za-z0-9_][A-Za-z0-9_.\-]*(/\.?[A-Za-z0-9_][A-Za-z0-9_.\-]*){0,3}$`) // allows .github/workflows/ci.yml; ".." never matches
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return strings.TrimRight(v, "/")
	}
	return d
}

type labInfo struct {
	Lab      string   `json:"lab"`
	Title    string   `json:"title"`
	Files    []file   `json:"files"`
	Editable []string `json:"editable"`
}
type file struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
type local struct {
	Lab      string   `json:"lab"`
	Editable []string `json:"editable"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "login":
		return login()
	case "logout":
		return os.RemoveAll(tokenPath())
	case "whoami":
		if _, err := token(); err != nil {
			return err
		}
		fmt.Println("Logged in to", site)
		return nil
	case "fetch":
		force := false
		var key string
		for _, a := range args[1:] {
			if a == "--force" {
				force = true
			} else {
				key = a
			}
		}
		return fetch(key, force)
	case "test":
		return localTest()
	case "submit":
		return submit()
	case "version", "--version", "-v":
		fmt.Println("gonomicon", version)
		return nil
	default:
		usage()
		return nil
	}
}

func usage() {
	fmt.Print(`gonomicon: GoNomicon lab tool

  gonomicon login                  connect this computer to your account
  gonomicon fetch <module>/<lab>   download a lab into a new folder (the lesson shows the exact command)
  gonomicon test                   run the lab's visible tests on your computer (needs Go)
  gonomicon submit                 send your work to be graded
  gonomicon logout
`)
}

// ---------- login ----------

func tokenPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "gonomicon", "token")
}

func token() (string, error) {
	b, err := os.ReadFile(tokenPath())
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return "", errors.New("you are not logged in. Run: gonomicon login")
	}
	return strings.TrimSpace(string(b)), nil
}

func call(method, path string, body any, tok string, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, site+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "gonomicon-cli/"+version)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("could not reach %s (%v). Check your internet connection", site, err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if out != nil {
		_ = json.Unmarshal(data, out)
	}
	if res.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("server returned %d", res.StatusCode)
		}
		return res.StatusCode, errors.New(e.Error)
	}
	return res.StatusCode, nil
}

func login() error {
	var d struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		URL        string `json:"verification_url"`
		Interval   int    `json:"interval"`
		ExpiresIn  int    `json:"expires_in"`
	}
	if _, err := call("POST", "/api/cli/device", map[string]string{}, "", &d); err != nil {
		return err
	}
	fmt.Printf("\n1. Open  %s\n2. Type this code:  %s\n\nWaiting for you to approve it in the browser...\n", d.URL, d.UserCode)
	wait := time.Duration(d.Interval) * time.Second
	if wait <= 0 || pollEvery < wait {
		wait = pollEvery
	}
	deadline := time.Now().Add(time.Duration(d.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(wait)
		var t struct {
			Token string `json:"token"`
		}
		if _, err := call("POST", "/api/cli/token", map[string]string{"device_code": d.DeviceCode}, "", &t); err != nil {
			return err
		}
		if t.Token != "" {
			if err := os.MkdirAll(filepath.Dir(tokenPath()), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(tokenPath(), []byte(t.Token), 0o600); err != nil {
				return err
			}
			fmt.Println("Logged in.")
			return nil
		}
	}
	return errors.New("that code expired. Run gonomicon login again")
}

// ---------- fetch ----------

func fetch(key string, force bool) error {
	if !labKeyRE.MatchString(key) {
		return errors.New("usage: gonomicon fetch <module>/<lab>   e.g. gonomicon fetch week-1/01-01-first-lab")
	}
	var info labInfo
	if _, err := call("GET", "/api/labs/"+key, nil, "", &info); err != nil {
		return err
	}
	name := key[strings.Index(key, "/")+1:]
	if _, err := os.Stat(name); err == nil && !force {
		return fmt.Errorf("folder %q already exists. Use --force to overwrite it (this replaces your edits)", name)
	}
	for _, f := range info.Files {
		if !safePathRE.MatchString(f.Path) {
			return fmt.Errorf("refusing unsafe file name %q", f.Path)
		}
		p := filepath.Join(name, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(f.Content), 0o644); err != nil {
			return err
		}
	}
	m, _ := json.Marshal(local{Lab: key, Editable: info.Editable})
	if err := os.WriteFile(filepath.Join(name, marker), m, 0o644); err != nil {
		return err
	}
	fmt.Printf("Downloaded %q into ./%s\nEdit: %s\nThen:  cd %s && gonomicon submit\n", info.Title, name, strings.Join(info.Editable, ", "), name)
	return nil
}

func readMarker() (local, error) {
	var l local
	b, err := os.ReadFile(marker)
	if err != nil {
		return l, errors.New("this folder is not a lab. Run the command inside a folder created by gonomicon fetch")
	}
	if json.Unmarshal(b, &l) != nil || !labKeyRE.MatchString(l.Lab) {
		return l, errors.New("the " + marker + " file is damaged. Fetch the lab again")
	}
	return l, nil
}

// ---------- test (local, visible tests only) ----------

// family looks at the file names gonomicon fetch wrote into this folder and
// guesses which language the lab is in, the same way the grader does
// server-side (see detectFamily in gonomicon-grader/run.mjs) — purely by
// extension/naming convention, so this needs no change every time a new
// track or language is added.
func family() string {
	entries, _ := os.ReadDir(".")
	has := func(suffix string) bool {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), suffix) {
				return true
			}
		}
		return false
	}
	switch {
	case has("_test.go"):
		return "go"
	case has(".c"), has(".h"):
		return "c"
	case has("Test.php"):
		return "php"
	case has(".py"):
		return "py"
	case has(".test.ts"), has(".test.tsx"), has(".test.js"), has(".test.jsx"):
		return "js"
	default:
		return "go"
	}
}

func localTest() error {
	if _, err := readMarker(); err != nil {
		return err
	}
	switch family() {
	case "go":
		return localTestGo()
	case "js":
		return localTestCmd("npx", []string{"vitest", "run"}, "Node.js (with npm)", "https://nodejs.org")
	case "py":
		return localTestPython()
	case "php":
		return localTestPhp()
	case "c":
		return localTestC()
	default:
		fmt.Println("Couldn't tell what language this lab is from its files. Submit to find out: gonomicon submit")
		return nil
	}
}

func localTestGo() error {
	goBin, err := exec.LookPath("go")
	if err != nil {
		fmt.Println("Go is not installed on this computer yet, so the local test can't run.")
		fmt.Println("You can still check your work: gonomicon submit")
		return nil
	}
	cmd := exec.Command(goBin, "test", "-count=1", "./...")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return errors.New("the visible test did not pass yet")
	}
	fmt.Println("Visible test passed. Submit to run the hidden checks: gonomicon submit")
	return nil
}

// localTestCmd runs one local command (its own toolchain's own test runner)
// and reports pass/fail the same way localTestGo does. Used by every
// non-Go, non-C family, since each of those just needs "run the right test
// command" rather than Go's own build step.
func localTestCmd(binName string, args []string, needLabel, installURL string) error {
	bin, err := exec.LookPath(binName)
	if err != nil {
		fmt.Printf("%s is not installed on this computer yet, so the local test can't run.\n", needLabel)
		fmt.Printf("Install it from %s, or just check your work: gonomicon submit\n", installURL)
		return nil
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return errors.New("the visible test did not pass yet")
	}
	fmt.Println("Visible test passed. Submit to run the hidden checks: gonomicon submit")
	return nil
}

func localTestPython() error {
	py, err := exec.LookPath("python3")
	if err != nil {
		py, err = exec.LookPath("python")
	}
	if err != nil {
		fmt.Println("Python is not installed on this computer yet, so the local test can't run.")
		fmt.Println("Install it from https://python.org, or just check your work: gonomicon submit")
		return nil
	}
	cmd := exec.Command(py, "-m", "pytest", "-q")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(err.Error(), "exit status 1") {
			return errors.New("the visible test did not pass yet")
		}
		fmt.Println("pytest isn't installed for this Python. Run: " + py + " -m pip install pytest")
		fmt.Println("Or just check your work: gonomicon submit")
		return nil
	}
	fmt.Println("Visible test passed. Submit to run the hidden checks: gonomicon submit")
	return nil
}

func localTestPhp() error {
	pest := filepath.Join("vendor", "bin", "pest")
	if _, err := os.Stat(pest); err != nil {
		fmt.Println("Pest isn't installed in this folder yet, so the local test can't run.")
		fmt.Println("Run: composer require --dev pestphp/pest && vendor/bin/pest --init")
		fmt.Println("Or just check your work: gonomicon submit")
		return nil
	}
	php, err := exec.LookPath("php")
	if err != nil {
		fmt.Println("PHP is not installed on this computer yet, so the local test can't run.")
		fmt.Println("Install it from https://php.net, or just check your work: gonomicon submit")
		return nil
	}
	cmd := exec.Command(php, pest)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return errors.New("the visible test did not pass yet")
	}
	fmt.Println("Visible test passed. Submit to run the hidden checks: gonomicon submit")
	return nil
}

func localTestC() error {
	cc, err := exec.LookPath("gcc")
	if err != nil {
		cc, err = exec.LookPath("clang")
	}
	if err != nil {
		fmt.Println("No C compiler (gcc or clang) found on this computer, so the local test can't run.")
		fmt.Println("Install one, or just check your work: gonomicon submit")
		return nil
	}
	entries, _ := os.ReadDir(".")
	var cFiles []string
	var visTest string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".c") {
			continue
		}
		if strings.HasPrefix(n, "test_") {
			visTest = n
			continue
		}
		if strings.HasPrefix(n, "hidden_test_") {
			continue // hidden test is never run locally — it's never fetched
		}
		cFiles = append(cFiles, n)
	}
	if visTest == "" {
		fmt.Println("Couldn't find this lab's visible test file. Submit to check your work: gonomicon submit")
		return nil
	}
	bin := filepath.Join(os.TempDir(), "gonomicon-local-test")
	args := append([]string{"-std=c17", "-Wall", "-Wextra", "-fsanitize=address,undefined", "-g", visTest}, cFiles...)
	args = append(args, "-o", bin, "-lpthread")
	build := exec.Command(cc, args...)
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		return errors.New("your code did not compile")
	}
	run := exec.Command(bin)
	run.Stdout, run.Stderr = os.Stdout, os.Stderr
	if err := run.Run(); err != nil {
		return errors.New("the visible test did not pass yet (or a sanitizer caught a bug — read the output above)")
	}
	fmt.Println("Visible test passed. Submit to run the hidden checks: gonomicon submit")
	return nil
}

// ---------- submit ----------

func submit() error {
	tok, err := token()
	if err != nil {
		return err
	}
	l, err := readMarker()
	if err != nil {
		return err
	}
	files := map[string]string{}
	for _, p := range l.Editable {
		b, err := os.ReadFile(filepath.FromSlash(p))
		if err != nil {
			return fmt.Errorf("cannot read %s: %v", p, err)
		}
		if len(b) > 20000 {
			return fmt.Errorf("%s is larger than 20 KB", p)
		}
		files[p] = string(b)
	}
	var s struct {
		ID string `json:"id"`
	}
	if _, err := call("POST", "/api/submit", map[string]any{"lab": l.Lab, "files": files}, tok, &s); err != nil {
		return err
	}
	fmt.Print("Submitted. Grading")
	deadline := time.Now().Add(pollFor)
	for time.Now().Before(deadline) {
		time.Sleep(pollEvery)
		var r struct {
			Status string `json:"status"`
			Output string `json:"output"`
		}
		if _, err := call("GET", "/api/submissions/"+s.ID, nil, tok, &r); err != nil {
			return err
		}
		switch r.Status {
		case "passed":
			fmt.Printf("\n\nPASSED\n%s\nYour progress is saved on your dashboard.\n", r.Output)
			return nil
		case "failed":
			fmt.Printf("\n\nNOT YET\n%s\n", r.Output)
			return errors.New("fix the problems above and run gonomicon submit again")
		case "error":
			fmt.Printf("\n\n%s\n", r.Output)
			return errors.New("the grader had a problem, not you. Try again in a minute")
		}
		fmt.Print(".")
	}
	return errors.New("grading is taking longer than usual. Check your dashboard in a minute")
}