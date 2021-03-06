/*
Copyright 2016 The Rook Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package mds

import (
	"fmt"

	"github.com/rook/rook/pkg/cephmgr/client"
	cephmds "github.com/rook/rook/pkg/cephmgr/mds"
	"github.com/rook/rook/pkg/cephmgr/mon"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/operator/k8sutil"
	k8smon "github.com/rook/rook/pkg/operator/mon"
	"k8s.io/client-go/1.5/kubernetes"
	"k8s.io/client-go/1.5/pkg/api/v1"
	extensions "k8s.io/client-go/1.5/pkg/apis/extensions/v1beta1"
)

const (
	appName            = "mds"
	dataPoolSuffix     = "-data"
	metadataPoolSuffix = "-metadata"
	keyringName        = "keyring"
)

type Cluster struct {
	Namespace string
	Version   string
	Replicas  int32
	factory   client.ConnectionFactory
}

func New(namespace, version string, factory client.ConnectionFactory) *Cluster {
	return &Cluster{
		Namespace: namespace,
		Version:   version,
		Replicas:  1,
		factory:   factory,
	}
}

func (c *Cluster) Start(clientset *kubernetes.Clientset, cluster *mon.ClusterInfo) error {
	logger.Infof("start running mds")

	if cluster == nil || len(cluster.Monitors) == 0 {
		return fmt.Errorf("missing mons to start mds")
	}

	context := &clusterd.DaemonContext{ConfigDir: k8sutil.DataDir}
	conn, err := mon.ConnectToClusterAsAdmin(clusterd.ToContext(context), c.factory, cluster)
	if err != nil {
		return fmt.Errorf("failed to connect to cluster as admin: %+v", err)
	}
	defer conn.Shutdown()

	id := "mds1"
	err = c.createKeyring(clientset, context, cluster, conn, id)
	if err != nil {
		return fmt.Errorf("failed to create mds keyring. %+v", err)
	}

	// start the deployment
	deployment, err := c.makeDeployment(cluster, id)
	_, err = clientset.Deployments(c.Namespace).Create(deployment)
	if err != nil {
		if !k8sutil.IsKubernetesResourceAlreadyExistError(err) {
			return fmt.Errorf("failed to create mds deployment. %+v", err)
		}
		logger.Infof("mds deployment already exists")
	} else {
		logger.Infof("mds deployment started")
	}

	return nil
}

func (c *Cluster) createKeyring(clientset *kubernetes.Clientset, context *clusterd.DaemonContext, cluster *mon.ClusterInfo, conn client.Connection, id string) error {
	_, err := clientset.Secrets(c.Namespace).Get(appName)
	if err == nil {
		logger.Infof("the mds keyring was already generated")
		return nil
	}
	if !k8sutil.IsKubernetesResourceNotFoundError(err) {
		return fmt.Errorf("failed to get mds secrets. %+v", err)
	}

	// get-or-create-key for the user account
	keyring, err := cephmds.CreateKeyring(conn, id)
	if err != nil {
		return fmt.Errorf("failed to create mds keyring. %+v", err)
	}

	// Store the keyring in a secret
	secrets := map[string]string{
		keyringName: keyring,
	}
	_, err = clientset.Secrets(c.Namespace).Create(&v1.Secret{ObjectMeta: v1.ObjectMeta{Name: appName}, StringData: secrets})
	if err != nil {
		return fmt.Errorf("failed to save mds secrets. %+v", err)
	}

	return nil
}

func (c *Cluster) makeDeployment(cluster *mon.ClusterInfo, id string) (*extensions.Deployment, error) {
	deployment := &extensions.Deployment{}
	deployment.Name = appName
	deployment.Namespace = c.Namespace

	podSpec := v1.PodTemplateSpec{
		ObjectMeta: v1.ObjectMeta{
			Name:        appName,
			Labels:      getLabels(cluster.Name),
			Annotations: map[string]string{},
		},
		Spec: v1.PodSpec{
			Containers:    []v1.Container{c.mdsContainer(cluster, id)},
			RestartPolicy: v1.RestartPolicyAlways,
			Volumes: []v1.Volume{
				{Name: k8sutil.DataDirVolume, VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}},
			},
		},
	}

	deployment.Spec = extensions.DeploymentSpec{Template: podSpec, Replicas: &c.Replicas}

	return deployment, nil
}

func (c *Cluster) mdsContainer(cluster *mon.ClusterInfo, id string) v1.Container {

	command := fmt.Sprintf("/usr/bin/rookd mds --data-dir=%s --mon-endpoints=%s --cluster-name=%s --mds-id=%s ",
		k8sutil.DataDir, mon.FlattenMonEndpoints(cluster.Monitors), cluster.Name, id)
	return v1.Container{
		// TODO: fix "sleep 5".
		// Without waiting some time, there is highly probable flakes in network setup.
		Command: []string{"/bin/sh", "-c", fmt.Sprintf("sleep 5; %s", command)},
		Name:    appName,
		Image:   k8sutil.MakeRookImage(c.Version),
		VolumeMounts: []v1.VolumeMount{
			{Name: k8sutil.DataDirVolume, MountPath: k8sutil.DataDir},
		},
		Env: []v1.EnvVar{
			{Name: "ROOKD_MDS_KEYRING", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{Name: appName}, Key: keyringName}}},
			k8smon.MonSecretEnvVar(),
			k8smon.AdminSecretEnvVar(),
		},
	}
}

func getLabels(clusterName string) map[string]string {
	return map[string]string{
		k8sutil.AppAttr:     appName,
		k8sutil.ClusterAttr: clusterName,
	}
}
