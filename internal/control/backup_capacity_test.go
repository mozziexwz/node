package control

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBackupCapacityRejectsAttachmentBeforeCommitAndStillRestores(t *testing.T) {
	a, _, _ := taskFixture(t)
	var admin *User
	if err := a.Store.Update(func(s *State) error {
		for _, u := range s.Users {
			if u.Role == "admin" {
				admin = u
			}
		}
		return SaveDoc(s, "articles", "capacity-article", Article{ID: "capacity-article", Title: "Capacity"})
	}); err != nil {
		t.Fatal(err)
	}
	accepted, rejected := 0, 0
	for i := 0; i < 6; i++ {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		file, err := form.CreateFormFile("file", fmt.Sprintf("file-%d.txt", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = file.Write(bytes.Repeat([]byte("x"), 10<<20)); err != nil {
			t.Fatal(err)
		}
		form.Close()
		r := httptest.NewRequest("POST", "/api/admin/articles/capacity-article/attachments", &body)
		r.Header.Set("Content-Type", form.FormDataContentType())
		r.SetPathValue("id", "capacity-article")
		r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, &requestIdentity{user: admin}))
		w := httptest.NewRecorder()
		a.uploadAttachment(w, r)
		switch w.Code {
		case 201:
			accepted++
		case 409:
			rejected++
			if !strings.Contains(w.Body.String(), "未保存") {
				t.Fatal(w.Body.String())
			}
		default:
			t.Fatalf("upload %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	if accepted != 4 || rejected != 2 {
		t.Fatalf("admission budget: %d accepted %d rejected", accepted, rejected)
	}
	if err := a.Store.View(func(s *State) error {
		if len(s.Docs["attachments"]) != accepted {
			t.Fatal("rejected attachment committed")
		}
		backup := NewBackupService(a)
		packed, err := backup.pack(s)
		if err != nil {
			return err
		}
		recovered, err := backup.unpack(packed)
		if err != nil {
			return err
		}
		if len(recovered.Docs["attachments"]) != accepted {
			t.Fatal("backup lost attachments")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBackupCapacityBudgetAndStateGrowthAreBounded(t *testing.T) {
	for _, n := range []int{0, 1, 1024, 10 << 20, backupMaxStateBytes} {
		if n <= backupMaxStateBytes && backupPackedBudget(n) > backupMaxBytes {
			t.Fatal("inconsistent export budget")
		}
	}
	if backupPackedBudget(backupMaxStateBytes+1024) <= backupMaxBytes {
		t.Fatal("size boundary not enforced")
	}
	a, _, _ := taskFixture(t)
	err := a.Store.Update(func(s *State) error {
		s.Settings["over-budget-state"] = strings.Repeat("x", backupMaxStateBytes+1024)
		return nil
	})
	if err != errBackupStateCapacity {
		t.Fatalf("oversized state admitted: %v", err)
	}
	_ = a.Store.View(func(s *State) error {
		if _, ok := s.Settings["over-budget-state"]; ok {
			t.Fatal("over-limit update committed")
		}
		return nil
	})
}
