package public

const maxConcurrentReports = 4

// Admission covers reading, signature verification, parsing and persistence so
// all copies of a large inventory body share the same bounded concurrency.
func (s *Repository) admitReport(ip string) (release func(), ok bool) {
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	if len(s.reporting) >= maxConcurrentReports {
		return nil, false
	}
	if _, active := s.reporting[ip]; active {
		return nil, false
	}
	if s.reporting == nil {
		s.reporting = make(map[string]struct{})
	}
	s.reporting[ip] = struct{}{}
	return func() {
		s.reportMu.Lock()
		delete(s.reporting, ip)
		s.reportMu.Unlock()
	}, true
}
