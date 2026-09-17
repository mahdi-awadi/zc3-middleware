package gym

import (
	"strings"
	"testing"
)

func TestBusinessDetailsAndLogo(t *testing.T) {
	path := t.TempDir() + "/gym.json"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetBusiness(Business{Name: "  أمد جم  ", Phone: "0770 000 0000", Address: " بغداد - الكرادة ", Note: "الاشتراك غير قابل للاسترجاع\r\nيُمنع إعارة البطاقة"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBusiness(Business{Name: strings.Repeat("x", 81)}); err == nil {
		t.Fatal("over-long name accepted")
	}
	// A plain config update must not wipe the business details.
	c := s.Config()
	c.Business = Business{}
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	png := []byte("\x89PNG\r\n\x1a\n0000")
	if err := s.SaveLogo(png); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b, hasLogo := s2.Business()
	if b.Name != "أمد جم" || b.Phone != "0770 000 0000" || b.Address != "بغداد - الكرادة" || b.Note != "الاشتراك غير قابل للاسترجاع\nيُمنع إعارة البطاقة" || !hasLogo {
		t.Fatalf("reloaded = %+v logo=%v", b, hasLogo)
	}
	if got, _ := s2.Logo(); string(got) != string(png) {
		t.Fatal("logo bytes changed")
	}
	if err := s2.DeleteLogo(); err != nil {
		t.Fatal(err)
	}
	if _, hasLogo := s2.Business(); hasLogo {
		t.Fatal("logo still present after delete")
	}
}
