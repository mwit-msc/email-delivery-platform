package edp

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
)

func TestEmailFontDocumentAndMIME(t *testing.T) {
	body := `<div style="font-family:'Noto Sans Thai',Tahoma,Arial,sans-serif"><p>เกียรติบัตร</p></div><style>@font-face{src:url(https://evil.example/font)}</style><script>evil()</script>`
	doc := emailDocument(body, "https://edp.example.test")
	if !strings.Contains(doc, `url("https://edp.example.test/assets/NotoSansThai.ttf")`) || strings.Contains(doc, "evil") || !strings.Contains(doc, "<!--[if mso]>") {
		t.Fatal("trusted font envelope missing or unsafe content retained")
	}
	if strings.Contains(emailDocument(`<p>Noto Sans Thai</p>`, "https://edp.example.test"), "@font-face") {
		t.Fatal("plain text mention caused font loading")
	}
	for _, base := range []string{"javascript:alert(1)", "https://user:pass@example.test", "https://example.test/</style><script>evil</script>"} {
		doc := emailDocument(body, base)
		if strings.Contains(doc, "<script>") || strings.Contains(doc, "javascript:") || strings.Contains(doc, "user:pass") {
			t.Fatal("public URL escaped its CSS envelope")
		}
	}
	message, err := mail.ReadMessage(bytes.NewReader(makeMIME(delivery{ID: "synthetic-font", From: "sender@example.test", Recipient: "recipient@example.test", Subject: "เกียรติบัตร", HTML: body, Plain: "เกียรติบัตรข้อความธรรมดา"}, "https://edp.example.test")))
	if err != nil {
		t.Fatal(err)
	}
	_, params, _ := mime.ParseMediaType(message.Header.Get("Content-Type"))
	parts := multipart.NewReader(message.Body, params["boundary"])
	for i := 0; i < 2; i++ {
		part, err := parts.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && string(data) != "เกียรติบัตรข้อความธรรมดา" {
			t.Fatal("font envelope modified the plain text alternative")
		}
		if i == 1 && (!strings.Contains(string(data), "@font-face") || !strings.Contains(string(data), "เกียรติบัตร")) {
			t.Fatal("SMTP MIME omitted the Thai font or message")
		}
	}
}

func TestPublicFontAllowsOpaqueSandboxOrigin(t *testing.T) {
	a := &App{}
	r := httptest.NewRequest("GET", emailFontPath, nil)
	r.Header.Set("Origin", "null")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("Access-Control-Allow-Origin") != "*" || w.Header().Get("Content-Type") != "font/ttf" || w.Body.Len() < 10000 {
		t.Fatal("sandboxed previews cannot retrieve the bundled font", w.Code, w.Header())
	}
}
