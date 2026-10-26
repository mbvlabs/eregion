package cli

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// answersFile is the optional `--answers` TOML. Every question in the guided
// flow has a key here and a flag; flags win. Secrets are never a key: the DSN
// is named by the path of a protected file.
type answersFile struct {
	Host            string `toml:"host"`
	Port            int    `toml:"port"`
	Identity        string `toml:"identity"`
	Node            string `toml:"node"`
	Context         string `toml:"context"`
	Hostname        string `toml:"hostname"`
	DatabaseURLFile string `toml:"database_url_file"`
	AdminUser       string `toml:"admin_user"`
}

// parseAnswersFile decodes the answers strictly: an unknown key is a typo, not
// a silently ignored answer.
func parseAnswersFile(raw []byte) (answersFile, error) {
	var answers answersFile
	decoder := toml.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&answers); err != nil {
		return answersFile{}, err
	}
	if answers.Port != 0 && (answers.Port < 1 || answers.Port > 65535) {
		return answersFile{}, fmt.Errorf("port must be from 1 to 65535")
	}
	return answers, nil
}

func (a answersFile) answers() serverInitAnswers {
	port := "22"
	if a.Port > 0 {
		port = fmt.Sprint(a.Port)
	}
	return serverInitAnswers{
		Host: a.Host, Port: port, Identity: strings.TrimSpace(a.Identity),
		Node: strings.TrimSpace(a.Node), Context: strings.TrimSpace(a.Context),
		Hostname: strings.TrimSpace(a.Hostname), DatabaseURLFile: strings.TrimSpace(a.DatabaseURLFile),
		AdminUser: strings.TrimSpace(a.AdminUser),
	}
}

// supplied reports which questions the file answered.
func (a answersFile) supplied() map[string]bool {
	return map[string]bool{
		"host":              a.Host != "",
		"identity":          a.Identity != "",
		"node":              a.Node != "",
		"context":           a.Context != "",
		"hostname":          a.Hostname != "",
		"database_url_file": a.DatabaseURLFile != "",
		"admin_user":        a.AdminUser != "",
	}
}
