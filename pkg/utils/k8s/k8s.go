package k8s

import (
	corev1 "k8s.io/api/core/v1"
)

// IsAvailable checks if both conditions ContainersReady and PodReady of a Pod are true.
func IsPodReady(pod corev1.Pod) bool {
	conditionsTrue := 0
	for _, cond := range pod.Status.Conditions {
		if cond.Status == corev1.ConditionTrue && (cond.Type == corev1.ContainersReady || cond.Type == corev1.PodReady) {
			conditionsTrue++
		}
	}
	return conditionsTrue == 2
}
