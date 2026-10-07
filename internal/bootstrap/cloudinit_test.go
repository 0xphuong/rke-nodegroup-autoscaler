package bootstrap

import (
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadParts(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"cloud-config": "#cloud-config\nusers: []\n",
		"init-00.sh":   "#! /bin/bash\necho base\n",
		"empty":        "\n",
		"..data":       "ignored (secret volume internals)",
	})
	parts, err := LoadParts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].Filename != "cloud-config" || parts[0].ContentType != "text/cloud-config" ||
		parts[1].Filename != "init-00.sh" || parts[1].ContentType != "text/x-shellscript" {
		t.Fatalf("parts = %+v", parts)
	}
	if p, err := LoadParts(filepath.Join(dir, "missing")); err != nil || p != nil {
		t.Fatalf("missing dir must mean no parts, got %v %v", p, err)
	}
	if _, err := LoadParts(writeFiles(t, map[string]string{"x": "apt-get update\n"})); err == nil {
		t.Fatal("a part that is neither #cloud-config nor #! must be refused")
	}
}

func TestUserDataMultipart(t *testing.T) {
	params := UserDataParams{
		NodeName: "dev-test-ng-abc123", Token: "tok", Endpoints: []string{"https://10.0.0.11:31443"},
		CACert: "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----", Deadline: 15 * time.Minute,
		Parts: []Part{
			{Filename: "cloud-config", ContentType: "text/cloud-config", Content: "#cloud-config\nusers:\n  - name: ops\n"},
			{Filename: "init-00.sh", ContentType: "text/x-shellscript", Content: "#! /bin/bash\napt-get install -y docker.io\n"},
		},
	}
	ud, err := UserData(params)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(ud))
	if err != nil {
		t.Fatalf("not a MIME document: %v", err)
	}
	mt, mp, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/mixed" {
		t.Fatalf("content type %q: %v", msg.Header.Get("Content-Type"), err)
	}
	r := multipart.NewReader(msg.Body, mp["boundary"])
	var got [][3]string
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(p)
		ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		got = append(got, [3]string{p.FileName(), ct, string(body)})
	}
	if len(got) != 3 {
		t.Fatalf("want 3 parts, got %d", len(got))
	}
	wantNames := []string{"00-cloud-config", "01-init-00.sh", "02-rke-nodegroup-join.sh"}
	wantTypes := []string{"text/cloud-config", "text/x-shellscript", "text/x-shellscript"}
	for i := range got {
		if got[i][0] != wantNames[i] || got[i][1] != wantTypes[i] {
			t.Errorf("part %d = %s %s, want %s %s", i, got[i][0], got[i][1], wantNames[i], wantTypes[i])
		}
	}
	join := got[2][2]
	if !strings.HasPrefix(join, "#!/bin/bash") || !strings.Contains(join, "Authorization: Bearer tok") {
		t.Errorf("last part must be the join script:\n%s", join)
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		if out, err := exec.Command(bash, "-n", "-c", join).CombinedOutput(); err != nil {
			t.Errorf("join part is not valid bash: %v\n%s", err, out)
		}
	}

	// without parts nothing changes: a plain script
	params.Parts = nil
	plain, _ := UserData(params)
	if !strings.HasPrefix(plain, "#!/bin/bash") {
		t.Errorf("without parts user_data must stay a plain script")
	}
}
