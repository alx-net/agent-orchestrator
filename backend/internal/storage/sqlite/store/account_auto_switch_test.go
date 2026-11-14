package store_test

import (
	"context"
	"testing"
	"time"
)

func TestAccountAutoSwitchIsOnByDefaultAndPersists(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	initial, err := s.GetAppSettings(ctx)
	if err != nil || !initial.AccountAutoSwitch {
		t.Fatalf("initial settings = %+v, %v; want the switch on", initial, err)
	}
	if err := s.SetAccountAutoSwitch(ctx, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	saved, err := s.GetAppSettings(ctx)
	if err != nil || saved.AccountAutoSwitch || !saved.VersionRefresh {
		t.Fatalf("saved settings = %+v, %v; want only the switch off", saved, err)
	}
}
