package algo

import (
	corev1 "k8s.io/api/core/v1"
)

type TolerationQueue struct {
	pods []*corev1.Pod
}

func NewTolerationQueue(pods []*corev1.Pod) *TolerationQueue {
	return &TolerationQueue{
		pods: pods,
	}
}

func (tol *TolerationQueue) Len() int      { return len(tol.pods) }
func (tol *TolerationQueue) Swap(i, j int) { tol.pods[i], tol.pods[j] = tol.pods[j], tol.pods[i] }
func (tol *TolerationQueue) Less(i, j int) bool {
	// NOTE: If pod i has tolerations configured, it's considered "less" (returning true for Less), implying lower priority in the sorting order.
	return tol.pods[i].Spec.Tolerations != nil
}
