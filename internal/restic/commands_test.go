package restic

import (
	"runtime"
	"strings"
	"testing"
)

func TestBuildSnapshotsArgv(t *testing.T) {
	argv, err := BuildSnapshotsArgv(SnapshotsOpts{
		Hosts: []string{"h1"},
		Tags:  []string{"daily"},
		Paths: []string{"/data"},
	})
	if err != nil {
		t.Fatal(err)
	}
	join := strings.Join(argv, " ")
	if !strings.HasPrefix(join, "snapshots --json") {
		t.Fatalf("argv: %v", argv)
	}
	if err := AssertArgvAllowed(argv); err != nil {
		t.Fatal(err)
	}
}

func TestBuildFindRejectsRegex(t *testing.T) {
	_, err := BuildFindArgv(FindOpts{Pattern: "foo|bar"})
	if err == nil {
		t.Fatal("expected reject")
	}
}

func TestBuildFindOK(t *testing.T) {
	argv, err := BuildFindArgv(FindOpts{Pattern: "*.txt", SnapshotIDs: []string{"abcdef0123456789"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := AssertArgvAllowed(argv); err != nil {
		t.Fatal(err)
	}
	// Pattern must be after end-of-flags marker.
	found := false
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == "--" && argv[i+1] == "*.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected -- before pattern in argv: %v", argv)
	}
}

func TestBuildFindRejectsFlagShapedPatterns(t *testing.T) {
	// M1: flag-shaped patterns must fail at BuildFindArgv (and would fail AssertArgvAllowed).
	bad := []string{
		"--repo=/tmp/evil",
		"--repository-file=/tmp/evil",
		"--password=secret",
		"--password-file=/tmp/p",
		"--cache-dir=/tmp/cache-hijack",
		"--insecure-tls",
		"--password-command=id",
		"-r",
		"--repo",
		"--no-lock",
		"-p",
		"--blob",
	}
	for _, p := range bad {
		argv, err := BuildFindArgv(FindOpts{Pattern: p})
		if err == nil {
			t.Fatalf("BuildFindArgv must reject pattern %q, got argv=%v", p, argv)
		}
		// Defense in depth: even if someone assembled argv manually, Assert must reject.
		manual := []string{"find", "--json", p}
		if err := AssertArgvAllowed(manual); err == nil {
			t.Fatalf("AssertArgvAllowed must reject token %q in %v", p, manual)
		}
	}
}

func TestAssertArgvRejectsRepoAndPasswordOverrides(t *testing.T) {
	cases := [][]string{
		{"snapshots", "--json", "--repo", "/evil"},
		{"snapshots", "--json", "--repo=/evil"},
		{"snapshots", "--json", "-r", "/evil"},
		{"snapshots", "--json", "-r=/evil"},
		{"find", "--json", "--repository-file=/evil"},
		{"find", "--json", "--password-file=/x"},
		{"find", "--json", "--password=x"},
		{"find", "--json", "--cache-dir=/x"},
		{"find", "--json", "--no-lock"},
		{"find", "--json", "--blob"},
		{"ls", "--json", "abcdef01", "--insecure-tls"},
	}
	for _, argv := range cases {
		if err := AssertArgvAllowed(argv); err == nil {
			t.Fatalf("expected reject for %v", argv)
		}
	}
}

func TestAssertArgvAllowsFindWithEndOfFlags(t *testing.T) {
	argv := []string{"find", "--json", "--", "*.txt"}
	if err := AssertArgvAllowed(argv); err != nil {
		t.Fatal(err)
	}
	// Flag-shaped positional after -- still rejected.
	if err := AssertArgvAllowed([]string{"find", "--json", "--", "--repo=/x"}); err == nil {
		t.Fatal("expected reject flag-shaped positional")
	}
}

func TestBuildLS(t *testing.T) {
	argv, err := BuildLSArgv(LSOpts{
		SnapshotID: "abcdef0123456789",
		Dirs:       []string{"/data"},
		Recursive:  false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := AssertArgvAllowed(argv); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildLSArgv(LSOpts{SnapshotID: "latest", Dirs: []string{"/data"}}); err == nil {
		t.Fatal("latest must be resolved before building ls argv")
	}
	if _, err := BuildLSArgv(LSOpts{SnapshotID: "abcdef0123456789", Dirs: []string{"/data/../etc"}}); err == nil {
		t.Fatal("parent segments must not reach ls argv")
	}
}

func TestBuildStatsModes(t *testing.T) {
	for _, m := range []StatsMode{StatsRestoreSize, StatsRawData, StatsFilesByContents, StatsBlobsPerFile} {
		argv, err := BuildStatsArgv(StatsOpts{Mode: m})
		if err != nil {
			t.Fatal(err)
		}
		if err := AssertArgvAllowed(argv); err != nil {
			t.Fatal(err)
		}
	}
	_, err := BuildStatsArgv(StatsOpts{Mode: "nope"})
	if err == nil {
		t.Fatal("expected invalid mode")
	}
	_, err = BuildStatsArgv(StatsOpts{Mode: StatsRestoreSize, SnapshotIDs: []string{"latest"}})
	if err == nil {
		t.Fatal("latest must be resolved before building stats argv")
	}
}

func TestAssertArgvRejectsForbidden(t *testing.T) {
	for _, sub := range []string{"backup", "restore", "forget", "prune", "unlock", "init", "check"} {
		if err := AssertArgvAllowed([]string{sub}); err == nil {
			t.Fatalf("should reject %s", sub)
		}
	}
}

func TestAssertArgvRejectsPasswordCommand(t *testing.T) {
	err := AssertArgvAllowed([]string{"snapshots", "--json", "--password-command", "cat /secret"})
	if err == nil {
		t.Fatal("expected reject")
	}
}

func TestValidSnapshotID(t *testing.T) {
	if !ValidSnapshotID("latest") {
		t.Fatal("latest")
	}
	if !ValidSnapshotID("abcdef01") {
		t.Fatal("prefix")
	}
	if ValidSnapshotID("short") {
		t.Fatal("too short")
	}
	if ValidSnapshotID("zzzzzzzz") {
		t.Fatal("non-hex")
	}
}

func TestChildEnvNoInheritance(t *testing.T) {
	env, err := BuildChildEnv(ChildEnvOpts{
		Repository:   "/srv/backups/repo",
		PasswordFile: "/run/secrets/pass",
		CacheDir:     "/var/cache/r",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "RESTIC_PASSWORD=") {
		t.Fatal("must not set RESTIC_PASSWORD")
	}
	if strings.Contains(joined, "RESTIC_PASSWORD_COMMAND") {
		t.Fatal("must not set password command")
	}
	if !strings.Contains(joined, "RESTIC_REPOSITORY=/srv/backups/repo") {
		t.Fatal("missing sealed repository location")
	}
	if strings.Contains(joined, "RESTIC_REPOSITORY_FILE=") {
		t.Fatal("repository file must not remain mutable after boot")
	}
	if !strings.Contains(joined, "RESTIC_PASSWORD_FILE=/run/secrets/pass") {
		t.Fatal("missing pass file")
	}
}

func TestChildEnvRejectsDuplicateBackendKeys(t *testing.T) {
	_, err := BuildChildEnv(ChildEnvOpts{
		Repository:   "/srv/backups/repo",
		PasswordFile: "/run/secrets/pass",
		ExtraEnv: []string{
			"AWS_REGION=eu-west-1",
			"AWS_REGION=us-east-1",
		},
	})
	if err == nil {
		t.Fatal("expected duplicate env key rejection")
	}
}

func TestMapExitCodes(t *testing.T) {
	cases := map[int]string{
		10:  "repository_unavailable",
		11:  "repository_locked",
		12:  "authentication_failed",
		130: "cancelled",
	}
	for code, want := range cases {
		err := MapExitCode(code, []byte("password: hunter2"))
		if err == nil {
			t.Fatalf("code %d", code)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("code %d: %v", code, err)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Fatal("password leaked in error")
		}
	}
}

func TestRedactChildStderrUsesSealedEnvValues(t *testing.T) {
	const repository = "rest:https://user:pass@example.test/private"
	raw := []byte("unable to open " + repository + " with token=abc123")
	safe := redactChildStderr(raw, []string{
		"PATH=/usr/bin:/bin",
		"RESTIC_REPOSITORY=" + repository,
	})
	if strings.Contains(string(safe), "example.test") || strings.Contains(string(safe), "abc123") {
		t.Fatalf("sensitive child stderr leaked: %s", safe)
	}
}

func TestCappedBufferStopsProducerAtLimit(t *testing.T) {
	called := 0
	buf := cappedBuffer{
		limit: 4,
		onExceeded: func() {
			called++
		},
	}
	if _, err := buf.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	if called != 0 {
		t.Fatal("callback fired before the limit was exceeded")
	}
	if _, err := buf.Write([]byte("5")); err != nil {
		t.Fatal(err)
	}
	if called != 1 || !buf.exceeded {
		t.Fatalf("callback=%d exceeded=%v", called, buf.exceeded)
	}
	if got := string(buf.Bytes()); got != "1234" {
		t.Fatalf("buffer=%q", got)
	}
}

func TestClassifyBackend(t *testing.T) {
	if ClassifyBackend("/var/backups/repo") != BackendLocal {
		t.Fatal("local")
	}
	if ClassifyBackend("relative/repo") != BackendOther {
		t.Fatal("relative local repositories must be rejected")
	}
	if runtime.GOOS != "windows" && ClassifyBackend(`C:\relative-on-unix`) != BackendOther {
		t.Fatal("Windows drive syntax must not be treated as absolute on Unix")
	}
	if ClassifyBackend("s3:s3.amazonaws.com/bucket") != BackendS3 {
		t.Fatal("s3")
	}
	if ClassifyBackend("sftp:user@host:/path") != BackendSFTP {
		t.Fatal("sftp")
	}
	if EnsureSupported(BackendSFTP) == nil {
		t.Fatal("sftp must be unsupported")
	}
	if EnsureSupported(BackendLocal) != nil {
		t.Fatal("local ok")
	}
}

func TestParseSnapshots(t *testing.T) {
	raw := `[
  {"time":"2024-01-02T03:04:05Z","hostname":"h1","paths":["/data"],"tags":["daily"],"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  {"time":"2024-01-03T03:04:05Z","hostname":"h1","paths":["/data"],"tags":["weekly"],"id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
]`
	snaps, err := ParseSnapshots([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("len %d", len(snaps))
	}
	// newest first
	if snaps[0].ID[0] != 'b' {
		t.Fatalf("order: %s", snaps[0].ID)
	}
}

func TestParseSnapshotsRejectsMalformedEntry(t *testing.T) {
	_, err := ParseSnapshots([]byte(`[{"time":"2024-01-02T03:04:05Z","id":"not-hex"}]`))
	if err == nil {
		t.Fatal("expected malformed snapshot rejection")
	}
}

func TestParseSnapshotsRejectsSanitizationBasedPolicyBypass(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cases := []string{
		`[{"hostname":"test\u001b[31mhost","paths":["/data"],"tags":["daily"],"id":"` + id + `"}]`,
		`[{"hostname":"testhost","paths":["/data/\u001b[31mvisible"],"tags":["daily"],"id":"` + id + `"}]`,
		`[{"hostname":"testhost","paths":["/data"],"tags":["dai\u001b[31mly"],"id":"` + id + `"}]`,
	}
	for _, raw := range cases {
		if _, err := ParseSnapshots([]byte(raw)); err == nil {
			t.Fatalf("expected policy-bearing control sequence rejection: %s", raw)
		}
	}
}

func TestParseLS(t *testing.T) {
	raw := `{"message_type":"snapshot","id":"aaa"}
{"message_type":"node","name":"f.txt","type":"file","path":"/data/f.txt","size":12,"permissions":"-rw-r--r--","mtime":"2024-01-01T00:00:00Z"}
{"message_type":"node","name":"d","type":"dir","path":"/data/d","permissions":"drwxr-xr-x","mtime":"2024-01-01T00:00:00Z"}
`
	nodes, trunc, err := ParseLS([]byte(raw), 10)
	if err != nil {
		t.Fatal(err)
	}
	if trunc || len(nodes) != 2 {
		t.Fatalf("nodes=%d trunc=%v", len(nodes), trunc)
	}
}

func TestParseLSRejectsInvalidPolicyPath(t *testing.T) {
	raw := "{\"message_type\":\"node\",\"name\":\"x\",\"type\":\"file\",\"path\":\"/data/\\u001b[31mx\"}\n"
	if _, _, err := ParseLS([]byte(raw), 10); err == nil {
		t.Fatal("expected invalid ls path rejection")
	}
}

func TestParseLSRejectsMalformedNode(t *testing.T) {
	_, _, err := ParseLS([]byte(`{"message_type":"node","name":"missing-path"}`+"\n"), 10)
	if err == nil {
		t.Fatal("expected malformed node rejection")
	}
}

func TestParseFind(t *testing.T) {
	raw := `[{"hits":1,"snapshot":"aaaaaaaa","matches":[{"path":"/data/f.txt","name":"f.txt","type":"file","size":1,"mtime":"2024-01-01T00:00:00Z"}]}]`
	g, total, trunc, err := ParseFind([]byte(raw), 10)
	if err != nil || trunc || total != 1 || len(g) != 1 {
		t.Fatalf("g=%v total=%d trunc=%v err=%v", g, total, trunc, err)
	}
}

func TestParseFindRejectsMalformedGroup(t *testing.T) {
	_, _, _, err := ParseFind([]byte(`[{"hits":1,"snapshot":"bad","matches":[]}]`), 10)
	if err == nil {
		t.Fatal("expected malformed find group rejection")
	}
}

func TestParseStats(t *testing.T) {
	raw := `{"total_size":100,"total_file_count":2,"snapshots_count":1}`
	s, err := ParseStats([]byte(raw))
	if err != nil || s.TotalSize != 100 {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestCompareVersion(t *testing.T) {
	if CompareVersion("0.17.0", MinResticVersion) >= 0 {
		t.Fatal("0.17.0 is below the supported minimum")
	}
	if CompareVersion("0.19.1", MinResticVersion) < 0 {
		t.Fatal("0.19 ok")
	}
}

func TestFakeRunnerRecordsArgv(t *testing.T) {
	f := NewFakeRunner()
	f.DefaultResult = &Result{Stdout: []byte("[]"), ExitCode: 0}
	_, err := f.Run(t.Context(), RunRequest{
		Argv: []string{"snapshots", "--json"},
		Env:  []string{"A=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := f.LastCall()
	if c == nil || c.Argv[0] != "snapshots" {
		t.Fatalf("%+v", c)
	}
}

func TestFakeRunnerRejectsBackup(t *testing.T) {
	f := NewFakeRunner()
	_, err := f.Run(t.Context(), RunRequest{Argv: []string{"backup", "/tmp"}})
	if err == nil {
		t.Fatal("expected reject")
	}
}

func FuzzParseSnapshots(f *testing.F) {
	f.Add([]byte(`[]`))
	f.Add([]byte(`[{"id":"aaaaaaaa","time":"2024-01-01T00:00:00Z","paths":["/x"],"hostname":"h"}]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseSnapshots(data)
	})
}

func FuzzParseLS(f *testing.F) {
	f.Add([]byte(`{"message_type":"node","path":"/a","name":"a","type":"file"}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = ParseLS(data, 10)
	})
}

func FuzzMapExit(f *testing.F) {
	f.Add(1, []byte("error password=secret"))
	f.Fuzz(func(t *testing.T, code int, stderr []byte) {
		_ = MapExitCode(code%200, stderr)
	})
}
