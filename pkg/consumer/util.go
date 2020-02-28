package consumer

import (
	"fmt"

	"golang.org/x/xerrors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

func WaitForFinish(client *kubernetes.Clientset, namespace, name string) (bool, error) {
	watchCh, err := client.CoreV1().Pods(namespace).Watch(metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", name),
	})
	if err != nil {
		return false, xerrors.Errorf(": %v", err)
	}
	defer watchCh.Stop()

	failed := false
Watch:
	for e := range watchCh.ResultChan() {
		switch e.Type {
		case watch.Modified:
			pod, ok := e.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			switch pod.Status.Phase {
			case corev1.PodSucceeded:
				break Watch
			case corev1.PodFailed:
				failed = true
				break Watch
			}
		}
	}

	return failed, nil
}
