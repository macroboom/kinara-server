package store

import "time"

// RequestsRevision is a number that changes whenever a request or an
// on-the-way status is added, changed or removed. It only means something
// compared with an earlier answer from the same run of the server: it starts
// from the clock, so a restart never repeats an old value.
func (s *Store) RequestsRevision() int64 {
	if v := s.requestsRev.Load(); v != 0 {
		return v
	}
	s.requestsRev.CompareAndSwap(0, time.Now().UnixMilli())
	return s.requestsRev.Load()
}

func (s *Store) requestsChanged() {
	s.RequestsRevision() // seeded first, so the bump moves off the clock value
	s.requestsRev.Add(1)
}

// LiveStatuses reports whether any on-the-way status hasn't expired yet.
func (s *Store) LiveStatuses() (bool, error) {
	var live bool
	err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM integ_status WHERE expires_at > ?)`, now()).Scan(&live)
	return live, err
}
