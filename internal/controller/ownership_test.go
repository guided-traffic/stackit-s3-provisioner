package controller

import (
	"strings"
	"testing"
)

func TestOwnerTagValue_StableAcrossUID(t *testing.T) {
	// The owner tag must be derived from namespace/name, NOT metadata.uid, so a
	// disaster-recovery restore that re-applies the same manifests (fresh UID) is
	// still recognized as the owner of its buckets.
	a := newBucket("team-a", "logs", "uid-old")
	b := newBucket("team-a", "logs", "uid-new-after-restore")

	if got := ownerTagValue(a); got != "team-a/logs" {
		t.Fatalf("ownerTagValue = %q, want %q", got, "team-a/logs")
	}
	if ownerTagValue(a) != ownerTagValue(b) {
		t.Fatalf("owner tag changed across UID: %q != %q", ownerTagValue(a), ownerTagValue(b))
	}
}

func TestOwnershipName_DefaultsWhenUnset(t *testing.T) {
	if got := (&BucketReconciler{}).ownershipName(); got != defaultOwnershipName {
		t.Fatalf("unset ownershipName = %q, want default %q", got, defaultOwnershipName)
	}
	if got := (&BucketReconciler{OwnershipName: "fleet-x"}).ownershipName(); got != "fleet-x" {
		t.Fatalf("configured ownershipName = %q, want %q", got, "fleet-x")
	}
}

func TestOwnershipTags(t *testing.T) {
	r := &BucketReconciler{OwnershipName: "fleet-x"}
	tags := r.ownershipTags(newBucket("ns", "nm", "uid"))

	if tags[tagOwnershipManagedBy] != "fleet-x" {
		t.Errorf("managed-by = %q, want %q", tags[tagOwnershipManagedBy], "fleet-x")
	}
	if tags[tagOwnershipOwner] != "ns/nm" {
		t.Errorf("owner = %q, want %q", tags[tagOwnershipOwner], "ns/nm")
	}
}

func TestIsOwnedByUs(t *testing.T) {
	b := newBucket("ns", "nm", "uid")

	cases := []struct {
		name          string
		ownershipName string
		tags          map[string]string
		want          bool
	}{
		{
			name:          "exact match",
			ownershipName: "fleet-x",
			tags:          map[string]string{tagOwnershipManagedBy: "fleet-x", tagOwnershipOwner: "ns/nm"},
			want:          true,
		},
		{
			name:          "default name matches default tag",
			ownershipName: "",
			tags:          map[string]string{tagOwnershipManagedBy: defaultOwnershipName, tagOwnershipOwner: "ns/nm"},
			want:          true,
		},
		{
			name:          "foreign fleet",
			ownershipName: "fleet-x",
			tags:          map[string]string{tagOwnershipManagedBy: "other-fleet", tagOwnershipOwner: "ns/nm"},
			want:          false,
		},
		{
			name:          "foreign owner (different CR)",
			ownershipName: "fleet-x",
			tags:          map[string]string{tagOwnershipManagedBy: "fleet-x", tagOwnershipOwner: "ns/other"},
			want:          false,
		},
		{
			name:          "untagged bucket",
			ownershipName: "fleet-x",
			tags:          map[string]string{},
			want:          false,
		},
		{
			name:          "managed-by present but owner missing",
			ownershipName: "fleet-x",
			tags:          map[string]string{tagOwnershipManagedBy: "fleet-x"},
			want:          false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &BucketReconciler{OwnershipName: tc.ownershipName}
			if got := r.isOwnedByUs(tc.tags, b); got != tc.want {
				t.Fatalf("isOwnedByUs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOwnershipCollisionError(t *testing.T) {
	err := &ownershipCollisionError{name: "bkt", detail: `owned by managed-by="x" owner="y"`}
	if msg := err.Error(); !strings.Contains(msg, "bkt") || !strings.Contains(msg, "not owned by this operator") {
		t.Fatalf("unexpected error message: %q", msg)
	}
}
