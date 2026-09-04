package asr

import (
	"testing"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
)

func TestRecognizersResolveSiteEndpoints(t *testing.T) {
	cred := common.NewCredential(1300000000, 1400000000, "secret")
	cred.SetSite(common.SiteIntl)

	speech := NewSpeechRecognizer(cred, "16k_zh", nil)
	ws, err := common.ResolveWSEndpoint(speech.endpoint, common.SiteOf(speech.credential))
	if err != nil {
		t.Fatal(err)
	}
	if ws != "wss://asr-intl.cloud-rtc.com" {
		t.Fatalf("speech intl endpoint = %q", ws)
	}

	speech.SetEndpoint("wss://127.0.0.1:9")
	ws, err = common.ResolveWSEndpoint(speech.endpoint, common.SiteOf(speech.credential))
	if err != nil {
		t.Fatal(err)
	}
	if ws != "wss://127.0.0.1:9" {
		t.Fatalf("speech override = %q", ws)
	}

	sent := NewSentenceRecognizer(cred)
	httpEP, err := common.ResolveHTTPEndpoint(sent.endpoint, common.SiteOf(sent.credential))
	if err != nil {
		t.Fatal(err)
	}
	if httpEP != "https://asr-intl.cloud-rtc.com" {
		t.Fatalf("sentence intl endpoint = %q", httpEP)
	}

	file := NewFileRecognizer(cred)
	httpEP, err = common.ResolveHTTPEndpoint(file.endpoint, common.SiteOf(file.credential))
	if err != nil {
		t.Fatal(err)
	}
	if httpEP != "https://asr-intl.cloud-rtc.com" {
		t.Fatalf("file intl endpoint = %q", httpEP)
	}
}

func TestDefaultSiteIsDomestic(t *testing.T) {
	cred := common.NewCredential(1, 2, "k")
	ws, err := common.ResolveWSEndpoint("", common.SiteOf(cred))
	if err != nil {
		t.Fatal(err)
	}
	if ws != "wss://asr.cloud-rtc.com" {
		t.Fatalf("default ws = %q", ws)
	}
}
