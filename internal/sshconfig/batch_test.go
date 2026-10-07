package sshconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const batchFixture = `[[connections]]
name = "alpha"
kind = "remote"
host = "alpha.example"
user = "tester"
password = "placeholder"
default_approval = true

[connections.jump]
host = "jump.example"
user = "tester"
password = "placeholder"

[[connections]]
name = "beta"
kind = "remote"
host = "beta.example"
user = "tester"
password = "placeholder"
`

func TestBatchImportExportAndTemporaryLifetime(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	result, err := s.ImportBatch([]byte(batchFixture), true)
	if err != nil || result.Imported != 2 || len(result.Renamed) != 0 {
		t.Fatalf("temporary import: result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ssh_configs")); !os.IsNotExist(err) {
		t.Fatalf("temporary import created a config directory: %v", err)
	}
	names, err := s.List()
	if err != nil || strings.Join(names, ",") != "alpha,beta,internal" {
		t.Fatalf("list: %v %v", names, err)
	}
	entry, err := s.Load("alpha")
	if err != nil || !entry.DefaultApproval || entry.Jump == nil || entry.Jump.Host != "jump.example" {
		t.Fatalf("temporary load lost fields: %+v %v", entry, err)
	}
	exported, err := s.ExportBatch(nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(exported), "internal") {
		t.Fatal("export included built-in internal profile")
	}
	restarted := NewStore(dir)
	if _, err := restarted.Load("alpha"); !errors.Is(err, ErrNotFound) {
		t.Fatal("temporary profile survived a new store")
	}
	result, err = restarted.ImportBatch(exported, false)
	if err != nil || result.Imported != 2 || len(result.Renamed) != 0 {
		t.Fatalf("persistent import of export: result=%+v err=%v\n%s", result, err, exported)
	}
	if _, err := os.Stat(filepath.Join(dir, "ssh_configs", "alpha", "config.toml")); err != nil {
		t.Fatal(err)
	}
	entry, err = NewStore(dir).Load("alpha")
	if err != nil || entry.Jump == nil || entry.Jump.Host != "jump.example" {
		t.Fatalf("persisted round trip lost jump: %+v %v", entry, err)
	}
}

func TestBatchImportRejectsInvalidFileAndRenamesCollisions(t *testing.T) {
	s := NewStore(t.TempDir())
	bad := strings.Replace(batchFixture, `host = "beta.example"`, `host = ""`, 1)
	if _, err := s.ImportBatch([]byte(bad), false); err == nil {
		t.Fatal("invalid second profile was accepted")
	}
	if names, _ := s.List(); len(names) != 1 {
		t.Fatalf("partial import after validation failure: %v", names)
	}
	if _, err := s.ImportBatch([]byte(batchFixture), false); err != nil {
		t.Fatal(err)
	}
	result, err := s.ImportBatch([]byte(batchFixture), true)
	if err != nil || len(result.Renamed) != 2 || result.Renamed[0] != (BatchRename{From: "alpha", To: "alpha-2"}) || result.Renamed[1] != (BatchRename{From: "beta", To: "beta-2"}) {
		t.Fatalf("collisions were not renamed: result=%+v err=%v", result, err)
	}
	entry, err := s.Load("alpha")
	if err != nil || entry.Host != "alpha.example" {
		t.Fatalf("collision changed existing entry: %+v %v", entry, err)
	}
	if !s.IsTemporary("alpha-2") || !s.IsTemporary("beta-2") {
		t.Fatal("renamed imported entries lost temporary storage mode")
	}
}

func TestBatchImportReservesSourceNamesAndTruncatesSuffix(t *testing.T) {
	s := NewStore(t.TempDir())
	raw := []byte("kind = \"remote\"\nhost = \"existing.example\"\nuser = \"tester\"\npassword = \"placeholder\"\n")
	if err := s.Save("alpha", raw); err != nil {
		t.Fatal(err)
	}
	file := strings.ReplaceAll(batchFixture, "name = \"beta\"", "name = \"alpha-2\"")
	result, err := s.ImportBatch([]byte(file), false)
	if err != nil || len(result.Renamed) != 1 || result.Renamed[0].To != "alpha-3" {
		t.Fatalf("reserved source name was taken: result=%+v err=%v", result, err)
	}
	if entry, err := s.Load("alpha-2"); err != nil || entry.Host != "beta.example" {
		t.Fatalf("original source name changed: %+v %v", entry, err)
	}
	longName := strings.Repeat("x", 64)
	longFile := strings.ReplaceAll(batchFixture, "name = \"alpha\"", "name = \""+longName+"\"")
	longFile = strings.ReplaceAll(longFile, "name = \"beta\"", "name = \""+longName+"\"")
	result, err = s.ImportBatch([]byte(longFile), true)
	if err != nil || len(result.Renamed) != 1 || len(result.Renamed[0].To) > 64 || !strings.HasSuffix(result.Renamed[0].To, "-2") {
		t.Fatalf("long duplicate was not shortened safely: result=%+v err=%v", result, err)
	}
}

func TestSaveWithOptionsSwitchesStorage(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	raw := []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"tester\"\npassword = \"placeholder\"\n")
	if err := s.Save("host", raw); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveWithOptions("host", raw, true); err != nil {
		t.Fatal(err)
	}
	if !s.IsTemporary("host") {
		t.Fatal("profile was not marked temporary")
	}
	if _, err := os.Stat(filepath.Join(dir, "ssh_configs", "host", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("persistent copy remained: %v", err)
	}
	if err := s.SaveWithOptions("host", raw, false); err != nil {
		t.Fatal(err)
	}
	if s.IsTemporary("host") {
		t.Fatal("profile remained temporary")
	}
	if err := s.Delete("host"); err != nil {
		t.Fatal(err)
	}
	if names, _ := s.List(); len(names) != 1 {
		t.Fatalf("delete left profile: %v", names)
	}
}
