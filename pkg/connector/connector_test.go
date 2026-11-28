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

func TestKomu(t *testing.T) {
	list := []edupage.Recipient{
		{ID: "StudTrieda7", Parents: "Trieda7", Name: "7.A", Kind: "class"},
		{ID: "StudentOnly502", Parents: "Student502", Name: "Bo Classmate", Kind: "student"},
		{ID: "Ucitel11", Name: "Bea Teacher", Kind: "teacher"},
	}
	lower, edu, err := pick(list, []string{"Trieda7", "ucitel11", "studentonly502", "UCITEL11", " "})
	if err != nil || strings.Join(lower, ";") != "studentonly502;trieda7;ucitel11" || strings.Join(edu, ";") != "StudentOnly502;Trieda7;Ucitel11" {
		t.Errorf("pick %v %v %v", lower, edu, err)
	}
	if name := komuName(list, lower); name != "Bo Classmate, 7.A (with parents), Bea Teacher" {
		t.Errorf("name %q", name)
	}
	if _, _, err = pick(list, []string{"StudTrieda7", "StudTrieda8"}); err == nil || !strings.Contains(err.Error(), "StudTrieda8") {
		t.Errorf("an id not offered was taken: %v", err)
	}
	if _, _, err = pick(list, nil); err == nil {
		t.Error("no recipients were taken")
	}
	if got, ok := portalKomu(komuPortal(lower)); !ok || strings.Join(got, ";") != strings.Join(lower, ";") {
		t.Errorf("portal %v", got)
	}
	if _, ok := portalKomu(groupPortal("trieda7")); ok {
		t.Error("a class room the poll made counts as a new message's room")
	}
	for ghost, want := range map[string]string{"student502": "StudentOnly502", "studentonly502": "StudentOnly502", "ucitel11": "Ucitel11", "trieda7": ""} {
		if got := sendID(makeUserID(ghost, "")); got != want {
			t.Errorf("%s writes to %q", ghost, got)
		}
	}
}
