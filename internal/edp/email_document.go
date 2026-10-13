package edp

import (
	"bytes"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"
)

const emailFontPath = "/assets/NotoSansThai.ttf"

var notoFamily = regexp.MustCompile(`(?i)(?:^|;)\s*font-family\s*:[^;]*\bNoto Sans Thai\b`)

// emailDocument adds application-owned font CSS after content sanitization.
// Templates cannot supply font URLs or arbitrary stylesheets.
func emailDocument(content, publicURL string) string {
	content = cleanHTML(content)
	font := false
	z := xhtml.NewTokenizer(strings.NewReader(content))
	for {
		kind := z.Next()
		if kind == xhtml.ErrorToken {
			break
		}
		if kind == xhtml.StartTagToken || kind == xhtml.SelfClosingTagToken {
			for _, attr := range z.Token().Attr {
				if attr.Key == "style" && notoFamily.MatchString(attr.Val) {
					font = true
				}
			}
		}
	}
	css := ""
	if font {
		u, err := url.Parse(publicURL)
		if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil {
			u.Path = strings.TrimRight(u.Path, "/") + emailFontPath
			u.RawPath, u.RawQuery, u.Fragment = "", "", ""
			css = `<style>@font-face{font-family:'Noto Sans Thai';src:url(` + strconv.Quote(u.String()) + `) format('truetype');font-style:normal;font-weight:100 900;font-display:swap}</style>` +
				`<!--[if mso]><style>body,table,td,p,a,span{font-family:Tahoma,Arial,sans-serif!important}</style><![endif]-->`
		}
	}
	return `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="color-scheme" content="light">` + css + `</head><body style="margin:0;padding:0">` + content + `</body></html>`
}

func emailPreviewCSP() string {
	return "sandbox; default-src 'none'; img-src https: http: data:; style-src 'unsafe-inline'; font-src 'self'"
}

// Sandboxed previews have an opaque origin, so this public licensed asset
// needs CORS. No credentials or application data are exposed by this route.
func serveEmailFont(w http.ResponseWriter, r *http.Request) {
	font, err := resources.ReadFile("web/assets/NotoSansThai.ttf")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "font/ttf")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Disposition", `inline; filename="NotoSansThai.ttf"`)
	http.ServeContent(w, r, "NotoSansThai.ttf", time.Time{}, bytes.NewReader(font))
}
