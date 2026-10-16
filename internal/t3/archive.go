package t3

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

var (
	ErrKVQuota         = errors.New("planner KV quota reached")
	ErrCapacity        = errors.New("planner capacity exhausted: no eligible closed record can be archived; preserve open plans and increase storage or export history")
	ErrArchive         = errors.New("planner archive unavailable or not durably verified; KV copy retained")
	ErrArchiveConflict = errors.New("planner archive conflicts with the retained record; both copies preserved")
)

const ArchiveRoot = "plans/archive"

type ArchiveEntry struct {
	Name         string
	Dir, Symlink bool
}
type Publication struct {
	Size    int64
	SHA256  string
	Durable bool
}

type ArchiveFiles interface {
	Read(path string) ([]byte, bool, error)
	Publish(path string, data []byte, expected string) (Publication, error)
	List(path string) ([]ArchiveEntry, bool, error)
}

func digest(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func ArchivePath(id string) string {
	h := digest([]byte(id))
	return ArchiveRoot + "/" + h[:2] + "/" + h + ".json"
}
func validArchivePath(path string) bool {
	prefix := ArchiveRoot + "/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rel := strings.TrimPrefix(path, prefix)
	if len(rel) != 72 || rel[2] != '/' || rel[67:] != ".json" || rel[:2] != rel[3:5] {
		return false
	}
	_, err := hex.DecodeString(rel[:2] + rel[3:67])
	return err == nil && strings.ToLower(rel) == rel
}
func (s *Service) archived(path string) (*Plan, bool, error) {
	if s.Files == nil {
		return nil, false, nil
	}
	raw, found, err := s.Files.Read(path)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	p, err := Decode(string(raw))
	if err != nil {
		return nil, false, err
	}
	if p.State != "closed" || ArchivePath(p.ID) != path {
		return nil, false, ErrArchiveConflict
	}
	p.archiveHash = digest(raw)
	return p, true, nil
}
func (s *Service) load(id string) (*Plan, error) {
	p, err := Load(s.KV, id)
	if !errors.Is(err, ErrNotFound) {
		return p, err
	}
	p, found, err := s.archived(ArchivePath(id))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	return p, nil
}
func (s *Service) exists(id string) (bool, error) {
	found, err := Exists(s.KV, id)
	if err != nil || found {
		return found, err
	}
	_, found, err = s.archived(ArchivePath(id))
	return found, err
}

func compatible(old, next *Plan) bool {
	if len(next.Advice) < len(old.Advice) {
		return false
	}
	for i, v := range old.Advice {
		a, _ := json.Marshal(v)
		b, _ := json.Marshal(next.Advice[i])
		if !bytes.Equal(a, b) {
			return false
		}
	}
	a, b := *old, *next
	a.Advice = nil
	b.Advice = nil
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func (s *Service) publish(p *Plan, expected string) error {
	if s.Files == nil || p.State != "closed" {
		return ErrArchive
	}
	if expected == "" {
		path := ArchivePath(p.ID)
		dir := path[:strings.LastIndex(path, "/")]
		entries, truncated, err := s.Files.List(dir)
		if err != nil {
			return err
		}
		if truncated || len(entries) >= 1024 {
			return ErrCapacity
		}
	}
	raw, err := Encode(p)
	if err != nil {
		return err
	}
	proof, err := s.Files.Publish(ArchivePath(p.ID), raw, expected)
	if err != nil {
		return err
	}
	if !proof.Durable || proof.Size != int64(len(raw)) || proof.SHA256 != digest(raw) {
		return ErrArchive
	}
	back, found, err := s.Files.Read(ArchivePath(p.ID))
	if err != nil {
		return err
	}
	if !found || !bytes.Equal(raw, back) {
		return ErrArchive
	}
	p.archiveHash = proof.SHA256
	return nil
}

func (s *Service) archiveOne(exclude string) error {
	if s.Files == nil {
		return ErrCapacity
	}
	keys, truncated, err := s.KV.List(KeyPrefix, MaxPlans)
	if err != nil {
		return err
	}
	if truncated {
		return ErrCapacity
	}
	var chosen *Plan
	for _, k := range keys {
		raw, found, err := s.KV.Get(k)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		p, err := decodeKey(k, raw)
		if err != nil {
			return err
		}
		if p.ID == exclude || p.State != "closed" {
			continue
		}
		if chosen == nil || p.ClosedMs < chosen.ClosedMs || (p.ClosedMs == chosen.ClosedMs && p.ID < chosen.ID) {
			chosen = p
		}
	}
	if chosen == nil {
		return ErrCapacity
	}
	old, found, err := s.archived(ArchivePath(chosen.ID))
	if err != nil {
		return err
	}
	expected := ""
	if found {
		if !compatible(old, chosen) {
			return ErrArchiveConflict
		}
		expected = old.archiveHash
	}

	if err = s.publish(chosen, expected); err != nil {
		return err
	}
	current, found, err := s.KV.Get(key(chosen.ID))
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	hot, err := decodeKey(key(chosen.ID), current)
	if err != nil {
		return err
	}
	if !compatible(chosen, hot) || !compatible(hot, chosen) {
		return ErrArchiveConflict
	}
	_, err = s.KV.Delete(key(chosen.ID))
	if err != nil {
		return err
	}
	_, exists, err := s.KV.Get(key(chosen.ID))
	if err != nil {
		return err
	}
	if exists {
		return ErrArchive
	}
	return nil
}
func (s *Service) save(p *Plan) error {
	if p.archiveHash != "" {
		old, found, err := s.archived(ArchivePath(p.ID))
		if err != nil {
			return err
		}
		if !found || old.archiveHash != p.archiveHash || !compatible(old, p) {
			return ErrArchiveConflict
		}
		return s.publish(p, p.archiveHash)
	}
	for i := 0; i <= MaxPlans; i++ {
		err := Save(s.KV, p)
		if !errors.Is(err, ErrKVQuota) {
			return err
		}
		if err = s.archiveOne(p.ID); err != nil {
			return err
		}
	}
	return ErrCapacity
}

func (s *Service) historyPage(family, after string) ([]*Plan, bool, string, error) {
	if !validToken(family, MaxIDBytes) {
		return nil, false, "", ErrInvalid
	}
	if !validHistoryCursor(after) {
		return nil, false, "", ErrInvalid
	}
	keys, truncated, err := s.KV.List(KeyPrefix, MaxPlans)
	if err != nil {
		return nil, false, "", err
	}
	if truncated {
		return nil, false, "", ErrCapacity
	}
	sort.Strings(keys)
	type item struct{ cursor, key, path string }
	items := make([]item, 0, MaxPlans+1)
	for _, k := range keys {
		id := strings.TrimPrefix(k, KeyPrefix)
		c := "h:" + id
		if after == "" || strings.HasPrefix(after, "h:") && c > after {
			items = append(items, item{cursor: c, key: k})
		}
	}
	if s.Files != nil && len(items) <= MaxPlans {
		dirs, more, err := s.Files.List(ArchiveRoot)
		if err != nil {
			return nil, false, "", err
		}
		if more {
			return nil, false, "", ErrArchive
		}
		sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
		for _, d := range dirs {
			if d.Symlink || !d.Dir || len(d.Name) != 2 {
				return nil, false, "", ErrArchiveConflict
			}
			if _, e := hex.DecodeString(d.Name); e != nil {
				return nil, false, "", ErrArchiveConflict
			}
			entries, more, err := s.Files.List(ArchiveRoot + "/" + d.Name)
			if err != nil {
				return nil, false, "", err
			}
			if more {
				return nil, false, "", ErrArchive
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
			for _, f := range entries {
				path := ArchiveRoot + "/" + d.Name + "/" + f.Name

				if !f.Dir && !f.Symlink && publicationTemporary(path) {
					continue
				}
				if f.Dir || f.Symlink || !validArchivePath(path) {
					return nil, false, "", ErrArchiveConflict
				}
				c := "f:" + path
				if strings.HasPrefix(after, "f:") && c <= after {
					continue
				}
				items = append(items, item{cursor: c, path: path})
				if len(items) > MaxPlans {
					break
				}
			}
			if len(items) > MaxPlans {
				break
			}
		}
	}
	more := len(items) > MaxPlans
	if more {
		items = items[:MaxPlans]
	}
	var out []*Plan
	next := ""
	for _, it := range items {
		next = it.cursor
		var p *Plan
		var err error
		if it.key != "" {
			raw, found, e := s.KV.Get(it.key)
			if e != nil {
				return nil, false, "", e
			}
			if !found {
				continue
			}
			p, err = decodeKey(it.key, raw)
		} else {
			var found bool
			p, found, err = s.archived(it.path)
			if err == nil && !found {
				continue
			}
			if err == nil {
				hot, e := Exists(s.KV, p.ID)
				if e != nil {
					return nil, false, "", e
				}
				if hot {
					continue
				}
			}
		}
		if err != nil {
			return nil, false, "", err
		}
		if p.State == "closed" && p.Family == family {
			out = append(out, p)
		}
	}
	if !more {
		next = ""
	}
	return out, more, next, nil
}

func validHistoryCursor(c string) bool {
	return c == "" || strings.HasPrefix(c, "h:") && validToken(c[2:], MaxIDBytes) || strings.HasPrefix(c, "f:") && validArchivePath(c[2:])
}

func publicationTemporary(path string) bool {
	parts := strings.Split(path, ".publish-")
	if len(parts) != 2 || !validArchivePath(parts[0]) || len(parts[1]) != 16 {
		return false
	}
	_, err := hex.DecodeString(parts[1])
	return err == nil
}
