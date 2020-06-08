package controllers

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

func podEquals(template1, template2 *v1.PodTemplateSpec) bool {
	// TODO this still sucks

	t1Copy := template1.DeepCopy()
	t2Copy := template2.DeepCopy()
	specs := []*v1.PodTemplateSpec{t1Copy, t2Copy}
	for i := range specs {
		delete(specs[i].Labels, "pod-template-hash")
		delete(specs[i].Labels, LabelColor)
		delete(specs[i].Labels, LabelName)

		spec := &specs[i].Spec

		if spec.RestartPolicy == "" {
			spec.RestartPolicy = "Always"
		}
		if spec.DNSPolicy == "" {
			spec.DNSPolicy = "ClusterFirst"
		}
		if spec.SchedulerName == "" {
			spec.SchedulerName = "default-scheduler"
		}
		if spec.SecurityContext == nil {
			spec.SecurityContext = &v1.PodSecurityContext{}
		}
		if spec.TerminationGracePeriodSeconds == nil {
			var sec int64 = 30
			spec.TerminationGracePeriodSeconds = &sec
		}
		for j := range spec.Containers {
			container := &spec.Containers[j]
			if container.TerminationMessagePath == "" {
				container.TerminationMessagePath = "/dev/termination-log"
			}
			if container.TerminationMessagePolicy == "" {
				container.TerminationMessagePolicy = "File"
			}

			for k := range container.Ports {
				port := &container.Ports[k]
				if port.Protocol == "" {
					port.Protocol = "TCP"
				}
			}
		}
	}
	return equality.Semantic.DeepEqual(t1Copy, t2Copy)
}

func svcEquals(svc1, svc2 *v1.ServiceSpec) bool {
	s1 := svc1.DeepCopy()
	s2 := svc2.DeepCopy()
	svcs := []*v1.ServiceSpec{s1, s2}
	for i := range svcs {
		s := svcs[i]
		s.Selector = nil
		s.ClusterIP = ""
		s.LoadBalancerIP = ""
		if s.SessionAffinity == "" {
			s.SessionAffinity = "None"
		}
		if s.ExternalTrafficPolicy == "" {
			s.ExternalTrafficPolicy = "Cluster"
		}
		for j := range s.Ports {
			p := &s.Ports[j]
			p.NodePort = 0
		}
	}
	return equality.Semantic.DeepEqual(s1, s2)
}
