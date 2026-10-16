package t3

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

type memoryArchive struct {
	files                               map[string][]byte
	failPublish, uncertain, corruptRead bool
	publishes                           int
}

func newArchive() *memoryArchive { return &memoryArchive{files: map[string][]byte{}} }
func (a *memoryArchive) Read(path string) ([]byte, bool, error) {
	v, ok := a.files[path]
	copy := bytes.Clone(v)
	if ok && a.corruptRead {
		copy = append(copy, 'x')
	}
	return copy, ok, nil
}
func (a *memoryArchive) Publish(path string, data []byte, expected string) (Publication, error) {
	if a.failPublish {
		return Publication{}, ErrArchive
	}
	old, exists := a.files[path]
	if expected == "" && exists || expected != "" && (!exists || digest(old) != expected) {
		return Publication{}, ErrArchiveConflict
	}
	a.files[path] = bytes.Clone(data)
	a.publishes++
	return Publication{Size: int64(len(data)), SHA256: digest(data), Durable: !a.uncertain}, nil
}
func (a *memoryArchive) List(path string) ([]ArchiveEntry, bool, error) {
	prefix := path + "/"
	seen := map[string]bool{}
	var result []ArchiveEntry
	for name := range a.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		first := strings.Split(rest, "/")[0]
		if seen[first] {
			continue
		}
		seen[first] = true
		result = append(result, ArchiveEntry{Name: first, Dir: strings.Contains(rest, "/")})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, false, nil
}
func closed(t *testing.T, s *Service, id string, at int64) {
	t.Helper()
	begin(t, s, id)
	if _, err := s.Apply(id, "a", "s1", "refinement", "a", "b", 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grade(id, "passed", at); err != nil {
		t.Fatal(err)
	}
}
func TestAutomaticArchivePreservesHistoryAndLocation(t *testing.T) {
	s, k := setup(t)
	a := newArchive()
	s.Files = a
	for i := 0; i < MaxPlans; i++ {
		closed(t, s, fmt.Sprintf("p%03d", i), int64(i+1))
	}
	before := k.rows[key("p000")]
	begin(t, s, "next")
	if _, ok := k.rows[key("p000")]; ok {
		t.Fatal("oldest closed row not reclaimed")
	}
	if !bytes.Equal(a.files[ArchivePath("p000")], []byte(before)) {
		t.Fatal("archive bytes differ")
	}
	for i := 0; i < MaxPlans; i++ {
		p, err := s.Material(fmt.Sprintf("p%03d", i))
		if err != nil || p.State != "closed" {
			t.Fatal(i, p, err)
		}
	}
	if _, err := s.Begin(Plan{ID: "p000", Family: "test", SessionID: "ws", FalsifierClass: "failed", ExpectedEvidenceClass: "passed", ConsequenceClass: "local", DeclaredSteps: 1, BudgetCalls: 1}, 4); !errors.Is(err, ErrExists) {
		t.Fatal("archived ID reused", err)
	}
	p, advice, err := s.Advise("p000", "control.advice_used")
	if err != nil || advice.SampleSize == 0 {
		t.Fatal(p, advice, err)
	}
	if _, hot := k.rows[key("p000")]; hot {
		t.Fatal("advice repopulated KV")
	}
	reloaded := NewService(k)
	reloaded.Files = a
	got, err := reloaded.Material("p000")
	if err != nil || len(got.Advice) != 1 || got.HostCalibration != "complete" {
		t.Fatal(got, err)
	}
}
func TestArchiveFailuresKeepKVAndResume(t *testing.T) {
	for _, failure := range []string{"publish", "uncertain", "readback"} {
		t.Run(failure, func(t *testing.T) {
			s, k := setup(t)
			a := newArchive()
			s.Files = a
			closed(t, s, "p", 1)
			before := k.rows[key("p")]
			switch failure {
			case "publish":
				a.failPublish = true
			case "uncertain":
				a.uncertain = true
			case "readback":
				a.corruptRead = true
			}
			if err := s.archiveOne(""); err == nil {
				t.Fatal("unverified archive accepted")
			}
			if k.rows[key("p")] != before {
				t.Fatal("KV changed after failed publication")
			}
			a.failPublish = false
			a.uncertain = false
			a.corruptRead = false
			if err := s.archiveOne(""); err != nil {
				t.Fatal(err)
			}
			if _, found := k.rows[key("p")]; found {
				t.Fatal("recovery did not reclaim")
			}
		})
	}
}

type deletionFailureKV struct {
	*memoryKV
	fail  bool
	after bool
}

func (k *deletionFailureKV) Delete(key string) (bool, error) {
	if k.fail {
		if k.after {
			delete(k.rows, key)
		}
		return false, errors.New("lost deletion acknowledgement")
	}
	return k.memoryKV.Delete(key)
}
func TestArchiveDeletionFailureIsRecoverable(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			s, k := setup(t)
			a := newArchive()
			s.Files = a
			closed(t, s, "p", 1)
			wrapped := &deletionFailureKV{memoryKV: k, fail: true, after: after}
			s.KV = wrapped
			if err := s.archiveOne(""); err == nil {
				t.Fatal("deletion error swallowed")
			}
			p, err := s.Material("p")
			if err != nil || p.State != "closed" {
				t.Fatal(p, err)
			}
			wrapped.fail = false
			if !after {
				if err = s.archiveOne(""); err != nil {
					t.Fatal(err)
				}
			}
			if _, found := k.rows[key("p")]; found {
				t.Fatal("hot copy remains")
			}
		})
	}
}
func TestArchiveConflictsKeepBothCopies(t *testing.T) {
	s, k := setup(t)
	a := newArchive()
	s.Files = a
	closed(t, s, "p", 1)
	p, _ := Load(k, "p")
	p.Family = "different"
	raw, _ := Encode(p)
	a.files[ArchivePath("p")] = raw
	before := k.rows[key("p")]
	if err := s.archiveOne(""); !errors.Is(err, ErrArchiveConflict) {
		t.Fatal(err)
	}
	if k.rows[key("p")] != before || !bytes.Equal(a.files[ArchivePath("p")], raw) {
		t.Fatal("conflict changed history")
	}
}
func TestAllOpenCapacityIsNotReclaimed(t *testing.T) {
	s, k := setup(t)
	a := newArchive()
	s.Files = a
	for i := 0; i < MaxPlans; i++ {
		begin(t, s, fmt.Sprintf("p%d", i))
	}
	_, err := s.Begin(Plan{ID: "next", Family: "test", SessionID: "ws", FalsifierClass: "failed", ExpectedEvidenceClass: "passed", ConsequenceClass: "local", DeclaredSteps: 1, BudgetCalls: 1}, 2)
	if !errors.Is(err, ErrCapacity) || planRows(k) != MaxPlans || len(a.files) != 0 {
		t.Fatal(err)
	}
}

type quotaKV struct {
	*memoryKV
	limit int
}

func (k *quotaKV) Put(key, value string) (PutResult, error) {
	total := len(value)
	for id, v := range k.rows {
		if id != key {
			total += len(v)
		}
	}
	if total > k.limit {
		return PutResult{}, ErrKVQuota
	}
	return k.memoryKV.Put(key, value)
}
func TestPublicByteQuotaReclaimsButObservationDoesNot(t *testing.T) {
	s, k := setup(t)
	a := newArchive()
	s.Files = a
	closed(t, s, "old", 1)
	wrapped := &quotaKV{memoryKV: k, limit: len(k.rows[key("old")]) + 400}
	s.KV = wrapped
	begin(t, s, "new")
	if len(a.files) != 1 {
		t.Fatal("quota did not archive")
	}
	wrapped.limit = 1
	before := a.publishes
	if err := s.Observe(event(1, "ws")); !errors.Is(err, ErrKVQuota) {
		t.Fatal(err)
	}
	if a.publishes != before {
		t.Fatal("observe archived outside its scope")
	}
}
func TestHistoryPagesAreBoundedAndComplete(t *testing.T) {
	s, k := setup(t)
	a := newArchive()
	s.Files = a
	// Seed valid immutable archive records without requiring a growing hot set.
	for i := 0; i < 225; i++ {
		closed(t, s, fmt.Sprintf("p%03d", i), int64(i+1))
		if err := s.archiveOne(""); err != nil {
			t.Fatal(err)
		}
	}
	if planRows(k) != 0 {
		t.Fatal("hot rows remain")
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		rows, more, next, err := s.historyPage("test", cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > MaxPlans {
			t.Fatal("unbounded page")
		}
		for _, p := range rows {
			if seen[p.ID] {
				t.Fatal("duplicate", p.ID)
			}
			seen[p.ID] = true
		}
		pages++
		if !more {
			break
		}
		if next == cursor || next == "" {
			t.Fatal("cursor did not progress")
		}
		cursor = next
	}
	if len(seen) != 225 || pages != 2 {
		t.Fatal(len(seen), pages)
	}
	// A duplicate during interrupted migration must be counted only once.
	raw := a.files[ArchivePath("p000")]
	var p Plan
	json.Unmarshal(raw, &p)
	if err := Save(k, &p); err != nil {
		t.Fatal(err)
	}
	rows, _, _, err := s.historyPage("test", "")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range rows {
		if p.ID == "p000" {
			n++
		}
	}
	if n != 1 {
		t.Fatal("migration duplicate", n)
	}
}

func TestArchiveRecoveryPreservesNewAdviceAndIgnoresUnpublishedSibling(t *testing.T) {
	s, k := setup(t)
	a := newArchive()
	s.Files = a
	closed(t, s, "p", 1)
	a.uncertain = true
	if err := s.archiveOne(""); err == nil {
		t.Fatal("uncertain publication accepted")
	}
	a.uncertain = false
	if _, _, err := s.Advise("p", "control.advice_used"); err != nil {
		t.Fatal(err)
	}
	if err := s.archiveOne(""); err != nil {
		t.Fatal(err)
	}
	a.files[ArchivePath("p")+".publish-0123456789abcdef"] = []byte("incomplete")
	rows, more, _, err := s.historyPage("test", "")
	if err != nil || more || len(rows) != 1 || len(rows[0].Advice) != 1 {
		t.Fatal(rows, more, err)
	}
	if planRows(k) != 0 {
		t.Fatal("KV copy remains")
	}
}
func TestArchiveRejectsMismatchedKVIdentity(t *testing.T) {
	s, k := setup(t)
	a := newArchive()
	s.Files = a
	closed(t, s, "p", 1)
	k.rows[key("other")] = k.rows[key("p")]
	delete(k.rows, key("p"))
	if err := s.archiveOne(""); err == nil {
		t.Fatal("mismatched KV identity archived")
	}
	if planRows(k) != 1 || len(a.files) != 0 {
		t.Fatal("corrupt record changed")
	}
}

func planRows(k *memoryKV) int {
	n := 0
	for key := range k.rows {
		if strings.HasPrefix(key, KeyPrefix) {
			n++
		}
	}
	return n
}
