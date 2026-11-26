package catalog

import "github.com/inkeliz/poudrine/internal/packageinfo"

// Keep the local archive in Compiled even when packagesite selects the upstream.
func cachePublication(upstream, local packageinfo.Package, publishLocal bool) *Snapshot {
	local.Local = true
	selected := upstream
	s := &Snapshot{Compiled: map[string]packageinfo.Package{local.Name: local}}
	if publishLocal {
		selected = local
		s.Displaced = map[string]packageinfo.Package{upstream.Name: upstream}
	}
	s.ByPath = map[string]packageinfo.Package{selected.Path: selected}
	s.PathByName = map[string]string{selected.Name: selected.Path}
	return s
}
