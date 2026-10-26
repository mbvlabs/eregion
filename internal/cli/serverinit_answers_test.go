package cli

import (
	"strings"
	"testing"
)

func TestAnswersFileParsesEveryQuestion(t *testing.T) {
	raw := []byte(`
host = "root@203.0.113.10:2222"
identity = "~/.ssh/id_ed25519"
node = "main"
context = "prod"
hostname = "app.example.com"
database_url_file = "/root/eregion-db.url"
admin_user = "deploy"
`)
	file, err := parseAnswersFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	answers := file.answers()
	if answers.Host != "root@203.0.113.10:2222" || answers.Identity != "~/.ssh/id_ed25519" ||
		answers.Node != "main" || answers.Context != "prod" || answers.Hostname != "app.example.com" ||
		answers.DatabaseURLFile != "/root/eregion-db.url" || answers.AdminUser != "deploy" {
		t.Fatalf("answers: %+v", answers)
	}
	for _, key := range []string{"host", "identity", "node", "context", "hostname", "database_url_file", "admin_user"} {
		if !file.supplied()[key] {
			t.Fatalf("%s was not reported as supplied", key)
		}
	}
}

func TestAnswersFileRejectsTyposAndBadPorts(t *testing.T) {
	if _, err := parseAnswersFile([]byte("node = \"main\"\nhotsname = \"app.example.com\"\n")); err == nil {
		t.Fatal("an unknown key was accepted")
	}
	if _, err := parseAnswersFile([]byte("port = 70000\n")); err == nil || !strings.Contains(err.Error(), "port") {
		t.Fatalf("out-of-range port: %v", err)
	}
}

func TestAnswersFileDefaultsPort(t *testing.T) {
	file, err := parseAnswersFile([]byte("node = \"main\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := file.answers().Port; got != "22" {
		t.Fatalf("default port %q", got)
	}
}
