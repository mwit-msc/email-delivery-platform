package edp

import (
	"context"
	"strings"
	"testing"
)

func TestLegacyEncodedTemplateURLsRenderWithoutRewritingVersions(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	stored := `<p>{{name}}</p><a href="%7B%7Bcertificate_url%7D%7D">Certificate</a><img src="%7B%7Blogo_url%7D%7D" width="96">`
	if _, err := a.DB.Exec(ctx, `UPDATE template_versions SET html=$1,variables=ARRAY['name','certificate_url','logo_url'] WHERE template_id='tpl_welcome' AND version=1`, stored); err != nil {
		t.Fatal(err)
	}
	variables := map[string]string{"name": "Synthetic", "certificate_url": "https://example.test/certificate?a=1&b=2", "logo_url": "https://example.test/logo.png"}
	rendered, err := a.renderMessage(ctx, "app_demo", "welcome", 1, variables)
	if err != nil || strings.Contains(rendered.HTML, "{{") || strings.Contains(rendered.HTML, "%7B") || !strings.Contains(rendered.HTML, `href="https://example.test/certificate?a=1&amp;b=2"`) || !strings.Contains(rendered.HTML, `src="https://example.test/logo.png"`) {
		t.Fatal("legacy certificate or logo URL failed to render", rendered.HTML, err)
	}
	var after string
	if err = a.DB.QueryRow(ctx, "SELECT html FROM template_versions WHERE template_id='tpl_welcome' AND version=1").Scan(&after); err != nil || after != stored {
		t.Fatal("rendering rewrote an immutable version", err)
	}
	variables["certificate_url"] = "javascript:alert(1)"
	if _, err = a.renderMessage(ctx, "app_demo", "welcome", 1, variables); err == nil {
		t.Fatal("legacy URL decoding bypassed URL validation")
	}
}
