package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/mosamlife/wpmgr/apps/api/internal/abilityrequest"
	"github.com/riverqueue/river"
)

func TestRegisterAbilityRequestContentRetentionWorker(t *testing.T) {
	workers := river.NewWorkers()
	periodics := registerAbilityRequestContentRetentionWorker(workers, nil, &abilityrequest.ContentRetentionWorker{})
	if len(periodics) != 1 {
		t.Fatalf("retention periodic jobs=%d", len(periodics))
	}
	if err := river.AddWorkerSafely(workers, &abilityrequest.ContentRetentionWorker{}); err == nil {
		t.Fatal("retention worker is not registered")
	}
	// Read the actual periodic configuration handed to River.
	job := reflect.ValueOf(periodics[0]).Elem()
	if !job.FieldByName("opts").Elem().FieldByName("RunOnStart").Bool() {
		t.Fatal("registered retention does not run on scheduler start")
	}
	interval := time.Duration(job.FieldByName("scheduleFunc").Elem().Elem().FieldByName("interval").Int())
	if interval != time.Hour {
		t.Fatalf("registered retention interval=%v, want %v", interval, time.Hour)
	}
}
