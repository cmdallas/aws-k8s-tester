package kubernetesconfig

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"text/template"
)

// Kubelet represents "kubelet" configurations.
type Kubelet struct {
	Path           string `json:"path"`
	DownloadURL    string `json:"download-url"`
	VersionCommand string `json:"version-command"`

	AllowPrivileged         bool   `json:"allow-privileged" kubelet:"allow-privileged"`
	AnonymousAuth           bool   `json:"anonymous-auth" kubelet:"anonymous-auth"`
	CgroupRoot              string `json:"cgroup-root" kubelet:"cgroup-root"`
	ClientCAFile            string `json:"client-ca-file" kubelet:"client-ca-file"`
	CloudProvider           string `json:"cloud-provider" kubelet:"cloud-provider"`
	ClusterDNS              string `json:"cluster-dns" kubelet:"cluster-dns"`
	ClusterDomain           string `json:"cluster-domain" kubelet:"cluster-domain"`
	EnableDebuggingHandlers bool   `json:"enable-debugging-handlers" kubelet:"enable-debugging-handlers"`
	EvictionHard            string `json:"eviction-hard" kubelet:"eviction-hard"`
	FeatureGates            string `json:"feature-gates" kubelet:"feature-gates"`
	HostnameOverride        string `json:"hostname-override" kubelet:"hostname-override"`
	Kubeconfig              string `json:"kubeconfig" kubelet:"kubeconfig"`
	NetworkPluginMTU        int64  `json:"network-plugin-mtu" kubelet:"network-plugin-mtu"`
	NetworkPlugin           string `json:"network-plugin" kubelet:"network-plugin"`
	NodeLabels              string `json:"node-labels" kubelet:"node-labels"`
	NonMasqueradeCIDR       string `json:"non-masquerade-cidr" kubelet:"non-masquerade-cidr"`
	PodInfraContainerImage  string `json:"pod-infra-container-image" kubelet:"pod-infra-container-image"`
	PodManifestPath         string `json:"pod-manifest-path" kubelet:"pod-manifest-path"`
	RegisterSchedulable     bool   `json:"register-schedulable" kubelet:"register-schedulable"`
	RegisterWithTaints      string `json:"register-with-taints" kubelet:"register-with-taints"`
	V                       int    `json:"v" kubelet:"v"`
	CNIBinDir               string `json:"cni-bin-dir" kubelet:"cni-bin-dir"`
	CNIConfDir              string `json:"cni-conf-dir" kubelet:"cni-conf-dir"`
}

var defaultKubeletMasterNodes = Kubelet{
	Path:           "/usr/bin/kubelet",
	DownloadURL:    fmt.Sprintf("https://storage.googleapis.com/kubernetes-release/release/v%s/bin/linux/amd64/kubelet", defaultKubernetesVersion),
	VersionCommand: "/usr/bin/kubelet --version",

	AllowPrivileged:         true,
	AnonymousAuth:           false,
	CgroupRoot:              "/",
	ClientCAFile:            "/srv/kubernetes/ca.crt",
	CloudProvider:           "aws",
	ClusterDNS:              "100.64.0.10", // ??
	ClusterDomain:           "cluster.local",
	EnableDebuggingHandlers: true,
	EvictionHard:            "memory.available<100Mi,nodefs.available<10%,nodefs.inodesFree<5%,imagefs.available<10%,imagefs.inodesFree<5%",
	FeatureGates:            "ExperimentalCriticalPodAnnotation=true",
	HostnameOverride:        "PRIVATE_DNS",
	Kubeconfig:              "/var/lib/kubelet/kubeconfig",
	NetworkPluginMTU:        9001,
	NetworkPlugin:           "kubenet",
	NodeLabels:              "aws-k8s-tester.k8s.io/instancegroup=master-us-west-2a,kubernetes.io/role=master,node-role.kubernetes.io/master=",
	NonMasqueradeCIDR:       "100.64.0.0/10",
	PodInfraContainerImage:  "k8s.gcr.io/pause-amd64:3.0",
	PodManifestPath:         "/etc/kubernetes/manifests",
	RegisterSchedulable:     true,
	RegisterWithTaints:      "node-role.kubernetes.io/master=:NoSchedule",
	V:                       2,
	CNIBinDir:               "/opt/cni/bin/",
	CNIConfDir:              "/etc/cni/net.d/",
}

func newDefaultKubeletMasterNodes() *Kubelet {
	copied := defaultKubeletMasterNodes
	return &copied
}

var defaultKubeletWorkerNodes = Kubelet{
	Path:           "/usr/bin/kubelet",
	DownloadURL:    fmt.Sprintf("https://storage.googleapis.com/kubernetes-release/release/v%s/bin/linux/amd64/kubelet", defaultKubernetesVersion),
	VersionCommand: "/usr/bin/kubelet --version",

	AllowPrivileged:         true,
	AnonymousAuth:           false,
	CgroupRoot:              "/",
	ClientCAFile:            "/srv/kubernetes/ca.crt",
	CloudProvider:           "aws",
	ClusterDNS:              "100.64.0.10", // ??
	ClusterDomain:           "cluster.local",
	EnableDebuggingHandlers: true,
	EvictionHard:            "memory.available<100Mi,nodefs.available<10%,nodefs.inodesFree<5%,imagefs.available<10%,imagefs.inodesFree<5%",
	FeatureGates:            "ExperimentalCriticalPodAnnotation=true",
	HostnameOverride:        "PRIVATE_DNS",
	Kubeconfig:              "/var/lib/kubelet/kubeconfig",
	NetworkPluginMTU:        9001,
	NetworkPlugin:           "kubenet",
	NodeLabels:              "aws-k8s-tester.k8s.io/instancegroup=nodes,kubernetes.io/role=node,node-role.kubernetes.io/node=",
	NonMasqueradeCIDR:       "100.64.0.0/10",
	PodInfraContainerImage:  "k8s.gcr.io/pause-amd64:3.0",
	PodManifestPath:         "/etc/kubernetes/manifests",
	RegisterSchedulable:     true,
	V:                       2,
	CNIBinDir:               "/opt/cni/bin/",
	CNIConfDir:              "/etc/cni/net.d/",
}

func newDefaultKubeletWorkerNodes() *Kubelet {
	copied := defaultKubeletWorkerNodes
	return &copied
}

// Service returns a script to configure Kubernetes Kubelet systemd service file.
func (kb *Kubelet) Service() (s string, err error) {
	tpl := template.Must(template.New("kubeletTemplate").Parse(kubeletTemplate))
	buf := bytes.NewBuffer(nil)
	kv := kubeletTemplateInfo{KubeletPath: kb.Path}
	if err := tpl.Execute(buf, kv); err != nil {
		return "", err
	}
	return buf.String(), nil
}

type kubeletTemplateInfo struct {
	KubeletPath string
}

const kubeletTemplate = `#!/usr/bin/env bash

sudo systemctl stop kubelet.service || true

sudo mkdir -p /etc/sysconfig/
sudo rm -f /etc/sysconfig/kubelet
sudo touch /etc/sysconfig/kubelet

sudo rm -rf /var/lib/kubelet/
sudo mkdir -p /var/lib/kubelet/
sudo rm -f /var/lib/kubelet/kubeconfig

rm -f /tmp/kubelet.service
cat <<EOF > /tmp/kubelet.service
[Unit]
Description=kubelet: The Kubernetes Node Agent
Documentation=http://kubernetes.io/docs/
After=docker.service

[Service]
EnvironmentFile=/etc/sysconfig/kubelet
ExecStart={{ .KubeletPath }} "\$DAEMON_ARGS"
Restart=always
RestartSec=2s
StartLimitInterval=0
KillMode=process
User=root

[Install]
WantedBy=multi-user.target
EOF
cat /tmp/kubelet.service

sudo mkdir -p /etc/systemd/system/kubelet.service.d
sudo cp /tmp/kubelet.service /etc/systemd/system/kubelet.service

sudo systemctl daemon-reload
sudo systemctl cat kubelet.service
`

// Flags returns the list of "kubelet" flags.
// Make sure to validate the configuration with "ValidateAndSetDefaults".
func (kb *Kubelet) Flags() (flags []string, err error) {
	tp, vv := reflect.TypeOf(kb).Elem(), reflect.ValueOf(kb).Elem()
	for i := 0; i < tp.NumField(); i++ {
		k := tp.Field(i).Tag.Get("kubelet")
		if k == "" {
			continue
		}
		allowZeroValue := tp.Field(i).Tag.Get("allow-zero-value") == "true"

		switch vv.Field(i).Type().Kind() {
		case reflect.String:
			if vv.Field(i).String() != "" {
				flags = append(flags, fmt.Sprintf("--%s=%s", k, vv.Field(i).String()))
			} else if allowZeroValue {
				flags = append(flags, fmt.Sprintf(`--%s=""`, k))
			}

		case reflect.Int, reflect.Int32, reflect.Int64:
			if vv.Field(i).String() != "" {
				flags = append(flags, fmt.Sprintf("--%s=%d", k, vv.Field(i).Int()))
			} else if allowZeroValue {
				flags = append(flags, fmt.Sprintf(`--%s=0`, k))
			}

		case reflect.Bool:
			flags = append(flags, fmt.Sprintf("--%s=%v", k, vv.Field(i).Bool()))

		default:
			return nil, fmt.Errorf("unknown %q", k)
		}
	}
	return flags, nil
}

// Sysconfig returns "/etc/sysconfig/kubelet" file.
func (kb *Kubelet) Sysconfig() (s string, err error) {
	var fs []string
	fs, err = kb.Flags()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`DAEMON_ARGS="%s"
HOME="/root"
`, strings.Join(fs, " ")), nil
}
