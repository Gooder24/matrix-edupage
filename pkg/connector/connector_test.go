package connector

import (
	"strings"
	"testing"
	"time"

	"github.com/Gooder24/matrix-edupage/pkg/edupage"
)

func TestIDs(t *testing.T) {
	for owner, want := range map[string]string{"Ucitel12": "Ucitel12", "StudentOnly501": "Student501", "Rodic3": "Rodic3", "Trieda7": ""} {
		if got := edupageID(makeUserID(owner, "")); got != want {
			t.Errorf("%s → %q", owner, got)
		}
	}
	if a, b := makeUserID("", "Name One"), makeUserID("", "Name Two"); a == b || len(a) != 12 {
		t.Errorf("name hashes %q %q", a, b)
	}
	if id, ok := portalPerson(personPortal("ucitel1")); !ok || id != "ucitel1" {
		t.Errorf("portal person %q", id)
	}
	if _, ok := portalPerson(groupPortal("trieda7")); ok {
		t.Error("a group counted as a person")
	}
	if parseMessageID(makeMessageID(4321)) != 4321 {
		t.Error("message id")
	}
}

func TestCleanSchool(t *testing.T) {
	for in, want := range map[string]string{"https://MySchool.edupage.org/user/": "myschool", "myschool": "myschool", "": ""} {
		if got, err := cleanSchool(in); err != nil || got != want {
			t.Errorf("%q → %q %v", in, got, err)
		}
	}
	if _, err := cleanSchool("not a school!"); err == nil {
		t.Error("a broken name was taken")
	}
}

func TestNoticeTexts(t *testing.T) {
	it := &edupage.Item{Type: "signin", RawData: []byte(`{"name":"Trip","typeName":"Course","time_from":"2026-12-01 08:00:00","time_to":"2026-12-05 16:00:00","targetNames":["A"]}`)}
	text := signInText(signIn(it))
	for _, want := range []string{"Prihlasovanie: Trip", "Course", "Otvorené 1. 12. 08:00 – 5. 12. 16:00", "Zvolené: A"} {
		if !strings.Contains(text, want) {
			t.Errorf("sign-up %q lacks %q", text, want)
		}
	}
	hw := &edupage.Item{Type: "homework", RawData: []byte(`{"date":"2026-11-27"}`)}
	if got := homeworkDue(hw); got != "Termín: piatok 27. 11." {
		t.Errorf("due %q", got)
	}
	if schoolNotice(&edupage.Item{Type: "sprava", Text: "x"}) || !schoolNotice(&edupage.Item{Type: "novy_typ", Text: "x"}) ||
		schoolNotice(&edupage.Item{Type: "novy_typ"}) || !schoolNotice(&edupage.Item{Type: "znamka"}) {
		t.Error("which items are notices")
	}
	if schoolYearStart(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)).Year() != 2025 {
		t.Error("school year")
	}
}
