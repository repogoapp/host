package vercel

import (
	"testing"

	"github.com/repogo/host/internal/clilogin"
)

func TestLoginLineReadsTheDevicePageAndItsCode(t *testing.T) {
	var code clilogin.Code
	if loginLine("Waiting for authentication...", &code) {
		t.Fatalf("a line without the page read as complete: %+v", code)
	}
	if !loginLine("  Visit https://vercel.com/oauth/device?user_code=ABCD-EFGH", &code) {
		t.Fatal("the Visit line did not complete the sign-in code")
	}
	if code.URL != "https://vercel.com/oauth/device?user_code=ABCD-EFGH" || code.UserCode != "ABCD-EFGH" {
		t.Fatalf("code = %+v", code)
	}
}

func TestToolIsACloudCLIWithInstallUpdateAndSignIn(t *testing.T) {
	tool := Tool()
	if tool.Kind != "vercel" || tool.Category != "cloud" || tool.Spec.Command != "vercel" || tool.Spec.Pkg != "vercel" {
		t.Fatalf("tool = %+v", tool)
	}
	if tool.Spec.LoginRead == nil || tool.Spec.AuthProbe == nil || len(tool.Capabilities) != 3 {
		t.Fatalf("sign-in is not wired: %+v", tool)
	}
}

func TestWhoamiReadsTheJSONAndTheOlderPlainUsername(t *testing.T) {
	ok, _, account := readWhoamiJSON([]byte(`{"username":"jhakim","email":"j@example.com","name":"Jerrick"}`))
	if !ok || account.Login != "jhakim" || account.Email != "j@example.com" || account.Name != "Jerrick" {
		t.Fatalf("json: ok=%v account=%+v", ok, account)
	}
	ok, _, account = readWhoamiLine([]byte("jhakim\n"))
	if !ok || account.Login != "jhakim" || account.Name != "jhakim" {
		t.Fatalf("plain: ok=%v account=%+v", ok, account)
	}
	if ok, _, _ := readWhoamiLine([]byte("")); ok {
		t.Fatal("empty output read as signed in")
	}
}
