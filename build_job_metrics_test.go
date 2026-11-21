package poudrine

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestConcurrentBuildJobsAndElapsed(t *testing.T) {
	path := t.TempDir()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(path, name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var ids []string
	for i := 1; i <= 24; i++ {
		id := fmt.Sprintf("%02d", i)
		ids = append(ids, id)
		write(".poudriere.status."+id, fmt.Sprintf("build:devel/p%s:p%s-1:%d:98", id, id, i))
	}
	write(".poudriere.status.25", "idle:")
	write(".poudriere.status.26", "build:www/nginx:nginx-2:101:101")
	write(".poudriere.builders", strings.Join(ids, " ")+" 25 ../illegal 26")
	var logs jobLogs
	jobs := runningJobs(root, 100, &logs)
	if len(jobs) != 25 || jobs[24].ElapsedSeconds != nil {
		t.Fatalf("unexpected jobs or future start %+v", jobs)
	}
	for i := range 24 {
		j := jobs[i]
		if j.ID != ids[i] || j.Package != "p"+ids[i]+"-1" || j.ElapsedSeconds == nil ||
			*j.ElapsedSeconds != int64(99-i) || j.ProgressPercent != nil || j.CPUPercent != nil || j.MemoryBytes != nil {
			t.Fatalf("job %d: %+v", i, j)
		}
	}
	// Phase changes do not restart the package clock, including across pauses.
	write(".poudriere.status.01", "package:devel/p01:p01-1:1:150")
	next := runningJobs(root, 151, &logs)
	if *next[0].ElapsedSeconds != 150 || next[0].Phase != "package" || next[0].ProgressPercent != nil {
		t.Fatalf("package phase: %+v", next[0])
	}
}

func TestCompilerProgressFollowsPoudrierePhase(t *testing.T) {
	path := t.TempDir()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Mkdir(filepath.Join(path, "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(path, "logs", "port-1.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var cursor jobLogCursor
	check := func(appendText string, want int) {
		t.Helper()
		if _, err := log.WriteString(appendText); err != nil {
			t.Fatal(err)
		}
		percent := cursor.progress(root, "port-1")
		if want < 0 && percent != nil || want >= 0 && (percent == nil || *percent != want) {
			t.Fatalf("after %q: got %v, want %d", appendText[:min(100, len(appendText))], percent, want)
		}
	}
	check("=======================<phase: configure      >============================\n[100%] test\n", -1)
	check("[00:05:09] =======================<phase: build          >============================\n[00:05:09] [  0%] start\n", 0)
	check("[00:05:10] [ 23%] Compiling a.c\n[00:05:11] [3/", 23)
	check("8] Compiling b.c\n", 37)
	check("download [71%]\n[10/0] bad total\n[56/5] bad count\n", 37)
	check(strings.Repeat("long line\n", 4000), 37)
	check("[00:06:00] =======================<phase: stage          >============================\n[3/4] fake\n", -1)
}

func TestBuilderJailResourceAttribution(t *testing.T) {
	jobs := []BuildJob{{ID: "01"}, {ID: "02"}, {ID: "03"}, {ID: "04"}, {ID: "05"}}
	owners := builderJails("latest-main", jobs, `0 latest-main-job-01
11 latest-main-job-01
12 latest-main-job-01-n
13 latest-main-job-01-n.child
14 latest-main-job-02
15 latest-main-job-03
16 latest-main-job-05
20 latest-main
21 other-main-job-01
22 latest-main-job-010
23 latest-main-job-01-unrelated
24 unrelated.latest-main-job-02`)
	if want := map[string]int{"11": 0, "12": 0, "13": 0, "14": 1, "15": 2, "16": 4}; !reflect.DeepEqual(owners, want) {
		t.Fatalf("jails %v, want %v", owners, want)
	}
	setJobResources(jobs, owners, `0 95 500
11 50.1 100
11 40.2 200
12 30.1 500
13 12.3 20
14 0.0 8
16 NaN 9
21 99.0 800`)
	for i, want := range []struct {
		cpu float64
		kib int64
	}{{132.7, 820}, {0, 8}, {0, 0}} {
		j := jobs[i]
		if j.CPUPercent == nil || *j.CPUPercent != want.cpu || j.MemoryBytes == nil || *j.MemoryBytes != want.kib*1024 {
			t.Errorf("job %d: %+v", i, j)
		}
	}
	for _, i := range []int{3, 4} {
		if jobs[i].CPUPercent != nil || jobs[i].MemoryBytes != nil {
			t.Errorf("unavailable sample %+v", jobs[i])
		}
	}
}

func TestPausedBuildJobSnapshot(t *testing.T) {
	worker := &Builder{status: BuildStatus{Paused: true, Jobs: []BuildJob{
		{Phase: "build", CPUPercent: new(120.0), MemoryBytes: new(int64(1024)), ProgressPercent: new(33)},
		{Phase: "starting"},
	}}}
	jobs := worker.Status().Jobs
	if *jobs[0].CPUPercent != 0 || jobs[1].CPUPercent != nil || *worker.status.Jobs[0].CPUPercent != 120 ||
		jobs[0].Phase != "build" || *jobs[0].ProgressPercent != 33 || *jobs[0].MemoryBytes != 1024 {
		t.Fatalf("paused jobs: %+v", jobs)
	}
}
