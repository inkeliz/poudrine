package poudrine

import "maps"

// The scanner runs independently of catalog signing. Keep its last errors for
// Management even when a read or signing failure prevented a snapshot swap.
type liveScanReport struct {
	failures map[string]liveCaptureFailure
	err      string
}

func (c *Catalog) recordLiveScan(failures map[string]liveCaptureFailure, scanErr error) {
	if scanErr == nil && len(failures) == 0 {
		c.liveScan.Store(nil)
		return
	}
	report := &liveScanReport{failures: maps.Clone(failures)}
	if scanErr != nil {
		report.err = scanErr.Error()
	}
	c.liveScan.Store(report)
}

func describeUnpublished(rows []managementPackage, s *catalogSnapshot, scan *liveScanReport) {
	for i := range rows {
		row := &rows[i]
		if !row.HistoryOnly {
			continue
		}
		if scan != nil {
			if failure, ok := scan.failures[row.Name+"-"+row.Version]; ok && (row.BuildOrigin == failure.origin || row.Origin == failure.origin) {
				row.PublicationNote = "Could not stage the finished archive (" + failure.message + ")."
			} else if row.PublicationNote == "" && scan.err != "" {
				row.PublicationNote = "The latest publication check failed (" + scan.err + ")."
			}
		}
		if row.PublicationNote != "" {
			continue
		}
		if s == nil {
			row.PublicationNote = "Waiting for the first verified repository catalog."
			continue
		}
		row.PublicationNote = "No matching compiled archive in the current catalog."
		if p, ok := s.byPath[s.pathByName[row.Name]]; ok && matchesBuildIdentity(p, row.BuildOrigin, row.BuildABI) {
			row.PublicationNote += " pkg currently offers " + p.Version + "."
		}
	}
}
