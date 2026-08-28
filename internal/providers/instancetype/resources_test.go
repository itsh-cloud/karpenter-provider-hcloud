package instancetype

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
)

// measured holds real figures read from live Hetzner nodes, in Ki as reported
// by the kubelet, and is the ground truth this package is calibrated against.
// capacity - allocatable is exactly 1560576Ki (512Mi kube + 512Mi system +
// 500Mi eviction) and 400m of CPU on every one of them.
var measured = []struct {
	name          string
	cores         int
	advertisedGiB float64
	diskGB        int
	capacityKi    int64
	allocatableKi int64
}{
	{"cx23", 2, 4, 40, 3918244, 2357668},
	{"cx33", 4, 8, 80, 7944076, 6383500},
	{"cpx32", 4, 8, 160, 7938240, 6377664},
	{"cx43", 8, 16, 160, 15995704, 14435128},
}

func ki(q resource.Quantity) int64 { return q.Value() / 1024 }

// TestCapacityNeverExceedsMeasured: over-estimating capacity makes Karpenter
// pack a node past what it holds, and the symptom is pods Pending on a node the
// scheduler thinks has room. Under-estimating merely wastes headroom, so the
// model must never be optimistic, for any type.
func TestCapacityNeverExceedsMeasured(t *testing.T) {
	for _, m := range measured {
		t.Run(m.name, func(t *testing.T) {
			c := Capacity(m.cores, m.advertisedGiB, m.diskGB, 110,
				DefaultVMMemoryOverheadPercent, DefaultVMDiskOverheadPercent)

			predicted := ki(*c.Memory())
			if predicted > m.capacityKi {
				t.Errorf("predicted memory capacity %dKi EXCEEDS measured %dKi; "+
					"over-estimating causes over-packing, raise DefaultVMMemoryOverheadPercent",
					predicted, m.capacityKi)
			}

			// Not so conservative that we waste a node. 10% slack is the most
			// that should ever be left on the table.
			if float64(predicted) < float64(m.capacityKi)*0.90 {
				t.Errorf("predicted memory capacity %dKi is more than 10%% below measured %dKi; "+
					"needlessly pessimistic", predicted, m.capacityKi)
			}

			// CPU is exact: the reserve is expressed as overhead, not here.
			if got := c.Cpu().Value(); got != int64(m.cores) {
				t.Errorf("cpu capacity = %d, want %d", got, m.cores)
			}
		})
	}
}

// TestAllocatableNeverExceedsMeasured is the same property one level down, and
// is the number Karpenter actually bin-packs against.
func TestAllocatableNeverExceedsMeasured(t *testing.T) {
	var kubelet v1alpha1.KubeletConfiguration // defaults

	for _, m := range measured {
		t.Run(m.name, func(t *testing.T) {
			c := Capacity(m.cores, m.advertisedGiB, m.diskGB, 110,
				DefaultVMMemoryOverheadPercent, DefaultVMDiskOverheadPercent)
			a := Allocatable(c, Overhead(&kubelet, c))

			predicted := ki(a[corev1.ResourceMemory])
			if predicted > m.allocatableKi {
				t.Errorf("predicted allocatable %dKi EXCEEDS measured %dKi", predicted, m.allocatableKi)
			}
			if float64(predicted) < float64(m.allocatableKi)*0.85 {
				t.Errorf("predicted allocatable %dKi is more than 15%% below measured %dKi",
					predicted, m.allocatableKi)
			}

			// The measured CPU reserve is exactly 400m on every node.
			wantMilli := int64(m.cores)*1000 - 400
			if got := a.Cpu().MilliValue(); got != wantMilli {
				t.Errorf("allocatable cpu = %dm, want %dm", got, wantMilli)
			}
		})
	}
}

// TestOverheadMatchesMeasuredReserve pins the 1560576Ki figure.
//
// If this drifts from what the kubelet is actually given, Karpenter's model
// and the node disagree and there is no alert for it.
func TestOverheadMatchesMeasuredReserve(t *testing.T) {
	var kubelet v1alpha1.KubeletConfiguration
	c := Capacity(8, 16, 160, 110, DefaultVMMemoryOverheadPercent, DefaultVMDiskOverheadPercent)
	o := Overhead(&kubelet, c)

	total := o.KubeReserved.Memory().Value() +
		o.SystemReserved.Memory().Value() +
		o.EvictionThreshold.Memory().Value()

	const wantKi = 1560576 // 512Mi + 512Mi + 500Mi, measured on every worker
	if got := total / 1024; got != wantKi {
		t.Errorf("memory overhead = %dKi, want %dKi (measured capacity-allocatable)", got, wantKi)
	}

	cpu := o.KubeReserved.Cpu().MilliValue() + o.SystemReserved.Cpu().MilliValue()
	if cpu != 400 {
		t.Errorf("cpu overhead = %dm, want 400m", cpu)
	}
}

func TestParseThreshold(t *testing.T) {
	total := resource.NewQuantity(100*(1<<30), resource.BinarySI) // 100 GiB

	tests := []struct {
		name    string
		value   string
		want    int64
		wantErr bool
	}{
		{"absolute", "500Mi", 500 * (1 << 20), false},
		{"percentage", "10%", 10 * (1 << 30), false},
		{"percentage with spaces", " 25% ", 25 * (1 << 30), false},
		{"zero percent", "0%", 0, false},
		{"bad percentage", "abc%", 0, true},
		{"out of range", "150%", 0, true},
		{"bad quantity", "not-a-size", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseThreshold(tt.value, total)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Value() != tt.want {
				t.Errorf("parseThreshold(%q) = %d, want %d", tt.value, got.Value(), tt.want)
			}
		})
	}
}

// TestEvictionThresholdIgnoresInodeSignals: inode pressure does not consume
// bytes, so subtracting it from allocatable would silently shrink every node.
func TestEvictionThresholdIgnoresInodeSignals(t *testing.T) {
	capacity := Capacity(8, 16, 160, 110, DefaultVMMemoryOverheadPercent, DefaultVMDiskOverheadPercent)

	out := evictionThreshold(map[string]string{
		"memory.available":   "500Mi",
		"nodefs.available":   "10%",
		"nodefs.inodesFree":  "5%",
		"imagefs.available":  "15%",
		"imagefs.inodesFree": "5%",
	}, capacity)

	if len(out) != 2 {
		t.Errorf("expected only memory and ephemeral-storage thresholds, got %v", out)
	}
	if got := out.Memory().Value(); got != 500*(1<<20) {
		t.Errorf("memory threshold = %d, want 500Mi", got)
	}
	// 10% of the ephemeral capacity, not of the advertised disk.
	capDisk := capacity[corev1.ResourceEphemeralStorage]
	wantDisk := int64(float64(capDisk.Value()) * 0.10)
	gotDisk := out[corev1.ResourceEphemeralStorage]
	if gotDisk.Value() < wantDisk-1 || gotDisk.Value() > wantDisk+1 {
		t.Errorf("nodefs threshold = %d, want ~%d", gotDisk.Value(), wantDisk)
	}
}

// TestEphemeralStorageIsDecimalGB guards the unit choice.
//
// Hetzner reports Disk in decimal GB. Treating it as GiB, as
// cluster-autoscaler does, over-states a 160 GB disk by about 7.4%.
func TestEphemeralStorageIsDecimalGB(t *testing.T) {
	c := Capacity(8, 16, 160, 110, DefaultVMMemoryOverheadPercent, 0)

	got := c[corev1.ResourceEphemeralStorage]
	if got.Value() != 160*1e9 {
		t.Errorf("ephemeral storage = %d, want %d (decimal GB)", got.Value(), int64(160*1e9))
	}
	if asGiB := int64(160) * (1 << 30); got.Value() >= asGiB {
		t.Errorf("ephemeral storage %d is at or above the GiB interpretation %d", got.Value(), asGiB)
	}
}
