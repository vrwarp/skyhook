package parity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const goodManifest = `{
  "id": "forms/select",
  "title": "a select change must reach the landside page",
  "gaps": ["P-101"],
  "waitText": "pick settled",
  "interactions": [
    {"do": "waitText", "value": "pick settled", "within": 15},
    {"do": "select", "target": "#pick", "value": "b"},
    {"do": "waitText", "value": "picked: b", "within": 10,
     "name": "change reaches page", "mustFail": true}
  ],
  "expectedFail": {"interaction": {"gap": "P-101", "reason": "select changes never reach the page"}}
}`

func TestManifestLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	writeFile(t, path, goodManifest)
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "forms/select" || len(m.Interactions) != 3 {
		t.Fatalf("loaded %+v", m)
	}
}

func TestManifestRejections(t *testing.T) {
	cases := []struct {
		name, json, want string
	}{
		{"unknown field", `{"id":"a/b","waitText":"x","surprise":1}`, "surprise"},
		{"no waitText", `{"id":"a/b"}`, "waitText"},
		{"bad id", `{"id":"nogroup","waitText":"x"}`, "<group>/<name>"},
		{"expectedFail on unknown dimension", `{"id":"a/b","waitText":"x",
			"expectedFail":{"vibes":{"gap":"P-001","reason":"r"}}}`, "vibes"},
		{"expectedFail on pixels", `{"id":"a/b","waitText":"x",
			"expectedFail":{"pixels":{"gap":"P-001","reason":"r"}}}`, "pixels"},
		{"expectedFail without a reason", `{"id":"a/b","waitText":"x",
			"expectedFail":{"text":{"gap":"P-001","reason":""}}}`, "reason"},
		{"exclude without a reason", `{"id":"a/b","waitText":"x",
			"exclude":{"pixels":""}}`, "why"},
		{"exclude and expectedFail together", `{"id":"a/b","waitText":"x",
			"exclude":{"text":"r"},
			"expectedFail":{"text":{"gap":"P-001","reason":"r"}}}`, "pick one"},
		{"unknown interaction", `{"id":"a/b","waitText":"x",
			"interactions":[{"do":"teleport"}]}`, "teleport"},
		{"mustFail with nothing positive before it", `{"id":"a/b","waitText":"x",
			"interactions":[{"do":"waitText","value":"y","name":"n","mustFail":true}]}`, "dead page"},
		{"mustFail without a name", `{"id":"a/b","waitText":"x",
			"interactions":[{"do":"waitText","value":"ok"},
			                {"do":"waitText","value":"y","mustFail":true}]}`, "named"},
		{"imported content without attribution", `{"id":"real/hn","waitText":"x"}`, "attribution"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.json")
			writeFile(t, path, tc.json)
			_, err := LoadManifest(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCorpusLoads(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "forms", "select", "manifest.json"), goodManifest)
	writeFile(t, filepath.Join(root, "forms", "select", "page.html"), "<p>pick settled</p>")
	pages, err := LoadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || pages[0].Manifest.ID != "forms/select" {
		t.Fatalf("loaded %v", pages)
	}
}

func TestCorpusHoldsItsShape(t *testing.T) {
	t.Run("id must match the directory", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "forms", "misnamed", "manifest.json"), goodManifest)
		writeFile(t, filepath.Join(root, "forms", "misnamed", "page.html"), "x")
		_, err := LoadCorpus(root)
		if err == nil || !strings.Contains(err.Error(), "directory says") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a page needs page.html", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "forms", "select", "manifest.json"), goodManifest)
		_, err := LoadCorpus(root)
		if err == nil || !strings.Contains(err.Error(), "page.html") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a declared CDN asset must exist", func(t *testing.T) {
		root := t.TempDir()
		m := strings.Replace(goodManifest, `"waitText": "pick settled",`,
			`"waitText": "pick settled", "serve": {"cdn": ["sprite.svg"]},`, 1)
		writeFile(t, filepath.Join(root, "forms", "select", "manifest.json"), m)
		writeFile(t, filepath.Join(root, "forms", "select", "page.html"), "x")
		_, err := LoadCorpus(root)
		if err == nil || !strings.Contains(err.Error(), "sprite.svg") {
			t.Fatalf("got %v", err)
		}
	})
}

const goodGaps = `{"gaps": [
  {"id": "P-101", "title": "select changes never reach the landside page",
   "status": "open", "refs": ["internal/mirror/agent.js:3461"]},
  {"id": "P-005", "title": "password values are never mirrored",
   "status": "by-design"},
  {"id": "P-024", "title": "file upload is not implemented",
   "status": "open", "noPage": "needs a real file chooser, which headless input cannot drive yet"}
]}`

func TestRegistryLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gaps.json")
	writeFile(t, path, goodGaps)
	r, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.Get("P-101") == nil || r.Get("P-999") != nil {
		t.Fatal("lookup misbehaves")
	}
}

func TestRegistryRejections(t *testing.T) {
	cases := []struct {
		name, json, want string
	}{
		{"bad id", `{"gaps":[{"id":"GAP-1","title":"t","status":"open"}]}`, "P-###"},
		{"duplicate id", `{"gaps":[{"id":"P-001","title":"t","status":"open"},
			{"id":"P-001","title":"t","status":"open"}]}`, "twice"},
		{"bad status", `{"gaps":[{"id":"P-001","title":"t","status":"sortafixed"}]}`, "status"},
		{"no title", `{"gaps":[{"id":"P-001","title":"","status":"open"}]}`, "title"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gaps.json")
			writeFile(t, path, tc.json)
			_, err := LoadRegistry(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRegistryAndCorpusHoldEachOtherToAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gaps.json")
	writeFile(t, path, goodGaps)
	reg, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}

	page := func(m Manifest) []CorpusPage {
		return []CorpusPage{{Manifest: &m}}
	}

	t.Run("the corpus measures what the registry admits", func(t *testing.T) {
		err := reg.CheckCorpus(page(Manifest{ID: "forms/select", WaitText: "x", Gaps: []string{"P-101"}}))
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a manifest cannot invent a gap", func(t *testing.T) {
		err := reg.CheckCorpus(page(Manifest{ID: "a/b", WaitText: "x", Gaps: []string{"P-777"}}))
		if err == nil || !strings.Contains(err.Error(), "P-777") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("an open gap must be measured or excused", func(t *testing.T) {
		err := reg.CheckCorpus(page(Manifest{ID: "a/b", WaitText: "x"}))
		if err == nil || !strings.Contains(err.Error(), "P-101") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("an expected failure must name a gap it lists", func(t *testing.T) {
		err := reg.CheckCorpus(page(Manifest{
			ID: "a/b", WaitText: "x", Gaps: []string{"P-101"},
			ExpectedFail: map[string]ExpectedFail{DimText: {Gap: "P-005", Reason: "r"}},
		}))
		if err == nil || !strings.Contains(err.Error(), "does not list") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a fixed gap cannot still be expected to fail", func(t *testing.T) {
		fixed := `{"gaps":[{"id":"P-001","title":"t","status":"fixed"}]}`
		p := filepath.Join(t.TempDir(), "gaps.json")
		writeFile(t, p, fixed)
		reg2, err := LoadRegistry(p)
		if err != nil {
			t.Fatal(err)
		}
		err = reg2.CheckCorpus(page(Manifest{
			ID: "a/b", WaitText: "x", Gaps: []string{"P-001"},
			ExpectedFail: map[string]ExpectedFail{DimText: {Gap: "P-001", Reason: "r"}},
		}))
		if err == nil || !strings.Contains(err.Error(), "fixed") {
			t.Fatalf("got %v", err)
		}
	})
}
