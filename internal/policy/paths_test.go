package policy

import "testing"

func TestCleanPath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"/data", "/data", false},
		{"/data/../etc", "", true},
		{"/data//x", "/data/x", false},
		{"relative", "", true},
		{"", "", true},
		{"/data/foo\\bar", "", true},
		{"/", "/", false},
	}
	for _, tc := range cases {
		got, err := CleanPath(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("CleanPath(%q) expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("CleanPath(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("CleanPath(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestPathAllowlist(t *testing.T) {
	pp := PathPolicy{Allowed: []string{"/srv/data", "/etc/app"}}
	if _, err := pp.Check("/srv/data/file"); err != nil {
		t.Fatal(err)
	}
	if _, err := pp.Check("/etc/app"); err != nil {
		t.Fatal(err)
	}
	if _, err := pp.Check("/etc/passwd"); err == nil {
		t.Fatal("expected deny")
	}
	if _, err := pp.Check("/srv"); err == nil {
		t.Fatal("expected deny parent")
	}
}

func TestEmptyAllowlistAllowsAll(t *testing.T) {
	pp := PathPolicy{}
	if _, err := pp.Check("/anything"); err != nil {
		t.Fatal(err)
	}
}

func TestFilterPathsProjectsBroaderSnapshotRoot(t *testing.T) {
	pp := PathPolicy{Allowed: []string{"/data/visible", "/etc/app"}}
	got := pp.FilterPaths([]string{"/data", "/etc/app/config", "/private"})
	if len(got) != 2 || got[0] != "/data/visible" || got[1] != "/etc/app/config" {
		t.Fatalf("visible paths: %v", got)
	}
}

func TestHostPolicy(t *testing.T) {
	hp := NewHostPolicy([]string{"alpha", "beta"})
	if err := hp.Check("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := hp.Check("gamma"); err == nil {
		t.Fatal("expected deny")
	}
}

func TestTagPolicy(t *testing.T) {
	tp := NewTagPolicy([]string{"daily", "weekly"})
	if err := tp.CheckEach([]string{"daily"}); err != nil {
		t.Fatal(err)
	}
	if err := tp.CheckEach([]string{"monthly"}); err == nil {
		t.Fatal("expected deny")
	}
}

func TestSnapshotVisible(t *testing.T) {
	hosts := NewHostPolicy([]string{"h1"})
	tags := NewTagPolicy([]string{"daily"})
	paths := PathPolicy{Allowed: []string{"/data"}}
	if !SnapshotVisible("h1", []string{"daily"}, []string{"/data/x"}, hosts, tags, paths) {
		t.Fatal("should be visible")
	}
	if SnapshotVisible("h2", []string{"daily"}, []string{"/data"}, hosts, tags, paths) {
		t.Fatal("wrong host")
	}
	if SnapshotVisible("h1", []string{"other"}, []string{"/data"}, hosts, tags, paths) {
		t.Fatal("wrong tag")
	}
	if SnapshotVisible("h1", []string{"daily"}, []string{"/other"}, hosts, tags, paths) {
		t.Fatal("wrong path")
	}
}

func FuzzCleanPath(f *testing.F) {
	f.Add("/data/file")
	f.Add("../etc/passwd")
	f.Add("/a/../../b")
	f.Fuzz(func(t *testing.T, p string) {
		_, _ = CleanPath(p)
	})
}
