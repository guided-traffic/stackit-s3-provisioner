package v1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fullBucket returns a Bucket with every nested field populated, so the
// generated DeepCopy implementations are exercised over all branches.
func fullBucket() *Bucket {
	return &Bucket{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "b",
			Namespace:   "ns",
			Annotations: map[string]string{ResolvedBucketNameAnnotation: "b"},
		},
		Spec: BucketSpec{
			BucketName:   "b",
			Region:       "eu01",
			WipeOnDelete: true,
			SecretRef: SecretReference{
				Name:      "sec",
				Namespace: "other",
				Keys: SecretKeys{
					AccessKeyID:     "AK",
					SecretAccessKey: "SK",
					BucketName:      "BN",
					Region:          "RG",
					Endpoint:        "EP",
					BucketURL:       "BU",
				},
			},
		},
		Status: BucketStatus{
			Phase:               PhaseReady,
			Message:             "ok",
			ObservedGeneration:  3,
			ResolvedBucketName:  "b",
			BucketURL:           "https://host/b",
			CredentialsGroupID:  "cg-1",
			CredentialsGroupURN: "urn:x",
			AccessKeyID:         "AK1",
			OperatorVersion:     "v1",
			Conditions: []metav1.Condition{{
				Type:   ConditionReady,
				Status: metav1.ConditionTrue,
				Reason: ReasonProvisioned,
			}},
		},
	}
}

func TestBucketDeepCopyFullObject(t *testing.T) {
	orig := fullBucket()
	cp := orig.DeepCopy()
	if cp == orig {
		t.Fatal("DeepCopy returned the same pointer")
	}
	// Mutating the copy must not leak into the original (maps/slices copied).
	cp.Annotations[ResolvedBucketNameAnnotation] = "changed"
	cp.Status.Conditions[0].Reason = ReasonFailed
	if orig.Annotations[ResolvedBucketNameAnnotation] != "b" {
		t.Error("annotation map is shared between original and copy")
	}
	if orig.Status.Conditions[0].Reason != ReasonProvisioned {
		t.Error("conditions slice is shared between original and copy")
	}

	if obj := orig.DeepCopyObject(); obj == nil {
		t.Error("DeepCopyObject returned nil")
	}
	var nilBucket *Bucket
	if nilBucket.DeepCopy() != nil {
		t.Error("DeepCopy of nil must be nil")
	}
}

func TestBucketListDeepCopy(t *testing.T) {
	list := &BucketList{Items: []Bucket{*fullBucket(), *fullBucket()}}
	cp := list.DeepCopy()
	if len(cp.Items) != 2 {
		t.Fatalf("copied list has %d items, want 2", len(cp.Items))
	}
	cp.Items[0].Spec.BucketName = "changed"
	if list.Items[0].Spec.BucketName != "b" {
		t.Error("items slice is shared between original and copy")
	}
	if obj := list.DeepCopyObject(); obj == nil {
		t.Error("DeepCopyObject returned nil")
	}
	var nilList *BucketList
	if nilList.DeepCopy() != nil {
		t.Error("DeepCopy of nil must be nil")
	}

	// Sub-structs' standalone DeepCopy variants.
	if fullBucket().Spec.DeepCopy() == nil ||
		fullBucket().Status.DeepCopy() == nil ||
		fullBucket().Spec.SecretRef.DeepCopy() == nil ||
		fullBucket().Spec.SecretRef.Keys.DeepCopy() == nil {
		t.Error("nested DeepCopy returned nil")
	}
}

func TestAuxiliaryDeepCopy(t *testing.T) {
	n := BucketNaming{Prefix: "p", IncludeNamespace: true}
	if cp := n.DeepCopy(); cp == nil || *cp != n {
		t.Errorf("BucketNaming.DeepCopy = %+v, want %+v", cp, n)
	}
	v := SecretValues{AccessKeyID: "AK", SecretAccessKey: "SK", Endpoint: "ep", BucketURL: "url"}
	if cp := v.DeepCopy(); cp == nil || *cp != v {
		t.Errorf("SecretValues.DeepCopy = %+v, want %+v", cp, v)
	}
	if (*BucketNaming)(nil).DeepCopy() != nil || (*SecretValues)(nil).DeepCopy() != nil {
		t.Error("DeepCopy of nil must be nil")
	}
}
