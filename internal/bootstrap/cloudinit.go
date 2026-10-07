package bootstrap

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Part is one document of a multi-part cloud-init user_data, e.g. the site's own #cloud-config (users, ssh) or
// a base setup script, which run before the provider's join script.
type Part struct {
	Filename    string
	ContentType string // text/cloud-config or text/x-shellscript
	Content     string
}

// LoadParts reads every regular file of dir, in name order. The content type comes from the first line:
// "#cloud-config" -> text/cloud-config, "#!" -> text/x-shellscript; anything else is an error. A missing dir
// means no extra parts.
func LoadParts(dir string) ([]Part, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		// Secret volumes add ..data symlinks and dot-dirs; real keys are symlinks to files
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var parts []Part
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		ct, err := contentType(b)
		if err != nil {
			return nil, fmt.Errorf("cloud-init part %s: %w", n, err)
		}
		parts = append(parts, Part{Filename: n, ContentType: ct, Content: string(b)})
	}
	return parts, nil
}

func contentType(b []byte) (string, error) {
	first := strings.TrimSpace(strings.SplitN(string(bytes.TrimLeft(b, " \t\r\n")), "\n", 2)[0])
	switch {
	case first == "#cloud-config":
		return "text/cloud-config", nil
	case strings.HasPrefix(first, "#!"):
		return "text/x-shellscript", nil
	}
	return "", fmt.Errorf("must start with #cloud-config or #! (got %q)", first)
}

// multipartUserData joins parts into a MIME multi-part document, the format cloud-init expects when user_data
// carries more than one document (what Terraform's cloudinit_config produces). Shell scripts run in the order
// given, after the cloud-config modules.
func multipartUserData(parts []Part) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	rnd := make([]byte, 12)
	if _, err := rand.Read(rnd); err != nil {
		return "", err
	}
	if err := w.SetBoundary("RKE-NG-" + hex.EncodeToString(rnd)); err != nil {
		return "", err
	}
	for i, p := range parts {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", p.ContentType+`; charset="utf-8"`)
		h.Set("Content-Transfer-Encoding", "7bit")
		// cloud-init runs x-shellscript parts sorted by filename: number them to keep the given order
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%02d-%s"`, i, p.Filename))
		pw, err := w.CreatePart(h)
		if err != nil {
			return "", err
		}
		if _, err := pw.Write([]byte(strings.TrimRight(p.Content, "\n") + "\n")); err != nil {
			return "", err
		}
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return fmt.Sprintf("Content-Type: multipart/mixed; boundary=%q\nMIME-Version: 1.0\n\n%s", w.Boundary(), body.String()), nil
}
