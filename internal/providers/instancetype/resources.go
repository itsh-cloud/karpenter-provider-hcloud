package instancetype

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/itsh-cloud/karpenter-provider-hcloud/api/v1alpha1"
)

const (
	// DefaultVMMemoryOverheadPercent is the fraction of advertised memory lost
	// to firmware, the hypervisor and kernel structures before the OS sees it.
	//
	// Hetzner advertises ServerType.Memory in units that are really GiB, but
	// the machine reports less, and the loss shrinks with size: 6.58% measured
	// on a cx23 down to 4.66% on a cx43. 7% sits just above the worst case
	// rather than at it, so every type is UNDER-estimated.
	//
	// The direction is the whole point. Under-estimating leaves a little
	// headroom unused; over-estimating makes Karpenter pack a node past what it
	// can hold, and the symptom is pods stuck Pending on a node the scheduler
	// believes has room, then memory pressure and NotReady flapping.
	DefaultVMMemoryOverheadPercent = 0.07

	// DefaultVMDiskOverheadPercent is the fraction of advertised disk not
	// available as ephemeral storage.
	//
	// Hetzner reports ServerType.Disk in DECIMAL GB and slightly over-delivers,
	// so unlike memory this needs no large correction. The small haircut covers
	// the filesystem overhead between the block device and what kubelet reports.
	DefaultVMDiskOverheadPercent = 0.02
)

// Capacity returns what a node of this server type reports as capacity, before
// the kubelet's reservations are subtracted.
func Capacity(cores int, memoryGiB float64, diskGB int, maxPods int32, memOverhead, diskOverhead float64) corev1.ResourceList {
	memBytes := memoryGiB * float64(1<<30) * (1 - memOverhead)
	diskBytes := float64(diskGB) * 1e9 * (1 - diskOverhead)

	return corev1.ResourceList{
		corev1.ResourceCPU:              *resource.NewQuantity(int64(cores), resource.DecimalSI),
		corev1.ResourceMemory:           *resource.NewQuantity(int64(memBytes), resource.BinarySI),
		corev1.ResourceEphemeralStorage: *resource.NewQuantity(int64(diskBytes), resource.DecimalSI),
		corev1.ResourcePods:             *resource.NewQuantity(int64(maxPods), resource.DecimalSI),
	}
}

// Overhead returns what the kubelet holds back from capacity.
//
// Karpenter computes allocatable as capacity minus these three, so they must be
// the numbers the kubelet is actually started with. Hence the single NodeClass
// kubelet block feeding both this and the rendered join configuration: if they
// diverge, Karpenter over-packs every node by the difference and nothing
// reports it.
func Overhead(kubelet *v1alpha1.KubeletConfiguration, capacity corev1.ResourceList) *cloudprovider.InstanceTypeOverhead {
	return &cloudprovider.InstanceTypeOverhead{
		KubeReserved:      kubelet.KubeReservedOrDefault(),
		SystemReserved:    kubelet.SystemReservedOrDefault(),
		EvictionThreshold: evictionThreshold(kubelet.EvictionHardOrDefault(), capacity),
	}
}

// evictionThreshold resolves the eviction signals Karpenter must subtract.
//
// Only the signals that reduce schedulable capacity are relevant: memory and
// the node filesystem. Percentage forms are resolved against the matching
// capacity, since InstanceTypeOverhead takes absolute quantities. Inode
// signals are ignored because they do not consume bytes.
func evictionThreshold(evictionHard map[string]string, capacity corev1.ResourceList) corev1.ResourceList {
	out := corev1.ResourceList{}

	if v, ok := evictionHard["memory.available"]; ok {
		if q, err := parseThreshold(v, capacity.Memory()); err == nil {
			out[corev1.ResourceMemory] = *q
		}
	}
	if v, ok := evictionHard["nodefs.available"]; ok {
		cap := capacity[corev1.ResourceEphemeralStorage]
		if q, err := parseThreshold(v, &cap); err == nil {
			out[corev1.ResourceEphemeralStorage] = *q
		}
	}

	return out
}

// parseThreshold accepts either an absolute quantity ("500Mi") or a percentage
// ("10%") resolved against total.
func parseThreshold(value string, total *resource.Quantity) (*resource.Quantity, error) {
	if pct, ok := strings.CutSuffix(strings.TrimSpace(value), "%"); ok {
		f, err := strconv.ParseFloat(pct, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing percentage %q: %w", value, err)
		}
		if f < 0 || f > 100 {
			return nil, fmt.Errorf("percentage %q out of range", value)
		}
		bytes := math.Ceil(float64(total.Value()) * f / 100)
		return resource.NewQuantity(int64(bytes), resource.BinarySI), nil
	}

	q, err := resource.ParseQuantity(value)
	if err != nil {
		return nil, fmt.Errorf("parsing quantity %q: %w", value, err)
	}
	return &q, nil
}

// Allocatable is capacity minus overhead, i.e. what Karpenter bin-packs
// against. Exposed for tests and for the instance-types debug command.
func Allocatable(capacity corev1.ResourceList, overhead *cloudprovider.InstanceTypeOverhead) corev1.ResourceList {
	out := capacity.DeepCopy()
	for _, sub := range []corev1.ResourceList{overhead.KubeReserved, overhead.SystemReserved, overhead.EvictionThreshold} {
		for name, q := range sub {
			if cur, ok := out[name]; ok {
				cur.Sub(q)
				out[name] = cur
			}
		}
	}
	return out
}
