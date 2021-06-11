/*
 Copyright 2021 Crunchy Data Solutions, Inc.
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

package pgbackrest

import (
	"strings"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/crunchydata/postgres-operator/internal/initialize"
	"github.com/crunchydata/postgres-operator/internal/naming"
	"github.com/crunchydata/postgres-operator/internal/postgres"
	"github.com/crunchydata/postgres-operator/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
)

// AddRepoVolumesToPod adds pgBackRest repository volumes to the provided Pod template spec, while
// also adding associated volume mounts to the containers specified.
func AddRepoVolumesToPod(postgresCluster *v1beta1.PostgresCluster, template *v1.PodTemplateSpec,
	containerNames ...string) error {

	for _, repo := range postgresCluster.Spec.Archive.PGBackRest.Repos {
		// we only care about repos created using PVCs
		if repo.Volume == nil {
			continue
		}
		repoVolName := repo.Name
		template.Spec.Volumes = append(template.Spec.Volumes, v1.Volume{
			Name: repoVolName,
			VolumeSource: v1.VolumeSource{
				PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
					ClaimName: naming.PGBackRestRepoVolume(postgresCluster,
						repoVolName).Name},
			},
		})

		for _, name := range containerNames {
			var containerFound bool
			var index int
			for index = range template.Spec.Containers {
				if template.Spec.Containers[index].Name == name {
					containerFound = true
					break
				}
			}
			if !containerFound {
				return errors.Errorf("Unable to find container %q when adding pgBackRest repo volumes",
					name)
			}
			template.Spec.Containers[index].VolumeMounts =
				append(template.Spec.Containers[index].VolumeMounts, v1.VolumeMount{
					Name:      repoVolName,
					MountPath: "/pgbackrest/" + repoVolName,
				})
		}
	}

	return nil
}

// AddConfigsToPod populates a Pod template Spec with with pgBackRest configuration volumes while
// then mounting that configuration to the specified containers.
func AddConfigsToPod(postgresCluster *v1beta1.PostgresCluster, template *v1.PodTemplateSpec,
	configName string, containerNames ...string) error {

	// grab user provided configs
	pgBackRestConfigs := postgresCluster.Spec.Archive.PGBackRest.Configuration
	// add default pgbackrest configs
	defaultConfig := v1.VolumeProjection{
		ConfigMap: &v1.ConfigMapProjection{
			LocalObjectReference: v1.LocalObjectReference{
				Name: naming.PGBackRestConfig(postgresCluster).Name,
			},
			Items: []v1.KeyToPath{
				{Key: configName, Path: configName},
				{Key: ConfigHashKey, Path: ConfigHashKey},
			},
		},
	}
	pgBackRestConfigs = append(pgBackRestConfigs, defaultConfig)

	template.Spec.Volumes = append(template.Spec.Volumes, v1.Volume{
		Name: ConfigVol,
		VolumeSource: v1.VolumeSource{
			Projected: &v1.ProjectedVolumeSource{
				Sources: pgBackRestConfigs,
			},
		},
	})

	for _, name := range containerNames {
		var containerFound bool
		var index int
		for index = range template.Spec.Containers {
			if template.Spec.Containers[index].Name == name {
				containerFound = true
				break
			}
		}
		if !containerFound {
			return errors.Errorf("Unable to find container %q when adding pgBackRest configration",
				name)
		}
		template.Spec.Containers[index].VolumeMounts =
			append(template.Spec.Containers[index].VolumeMounts,
				v1.VolumeMount{
					Name:      ConfigVol,
					MountPath: ConfigDir,
				})
	}

	return nil
}

// AddSSHToPod populates a Pod template Spec with with the container and volumes needed to enable
// SSH within a Pod.  It will also mount the SSH configuration to any additional containers specified.
func AddSSHToPod(postgresCluster *v1beta1.PostgresCluster, template *v1.PodTemplateSpec,
	enableSSHD bool, resources v1.ResourceRequirements,
	additionalVolumeMountContainers ...string) error {

	sshConfigs := []v1.VolumeProjection{}
	// stores all SSH configurations (ConfigMaps & Secrets)
	if postgresCluster.Spec.Archive.PGBackRest.RepoHost.SSHConfiguration == nil {
		sshConfigs = append(sshConfigs, v1.VolumeProjection{
			ConfigMap: &v1.ConfigMapProjection{
				LocalObjectReference: v1.LocalObjectReference{
					Name: naming.PGBackRestSSHConfig(postgresCluster).Name,
				},
			},
		})
	} else {
		sshConfigs = append(sshConfigs, v1.VolumeProjection{
			ConfigMap: postgresCluster.Spec.Archive.PGBackRest.RepoHost.SSHConfiguration,
		})
	}
	if postgresCluster.Spec.Archive.PGBackRest.RepoHost.SSHSecret == nil {
		sshConfigs = append(sshConfigs, v1.VolumeProjection{
			Secret: &v1.SecretProjection{
				LocalObjectReference: v1.LocalObjectReference{
					Name: naming.PGBackRestSSHSecret(postgresCluster).Name,
				},
			},
		})
	} else {
		sshConfigs = append(sshConfigs, v1.VolumeProjection{
			Secret: postgresCluster.Spec.Archive.PGBackRest.RepoHost.SSHSecret,
		})
	}
	template.Spec.Volumes = append(template.Spec.Volumes, v1.Volume{
		Name: naming.PGBackRestSSHVolume,
		VolumeSource: v1.VolumeSource{
			Projected: &v1.ProjectedVolumeSource{
				Sources:     sshConfigs,
				DefaultMode: initialize.Int32(0o040),
			},
		},
	})

	sshVolumeMount := v1.VolumeMount{
		Name:      naming.PGBackRestSSHVolume,
		MountPath: sshConfigPath,
		ReadOnly:  true,
	}

	// Only add the SSHD container if requested.  Sometimes (e.g. when running a restore Job) it is
	// not necessary to run a full SSHD server, but the various SSH configs are still needed.
	if enableSSHD {
		container := v1.Container{
			Command: []string{"/usr/sbin/sshd", "-D", "-e"},
			Image:   postgresCluster.Spec.Archive.PGBackRest.Image,
			LivenessProbe: &v1.Probe{
				Handler: v1.Handler{
					TCPSocket: &v1.TCPSocketAction{
						Port: intstr.FromInt(2022),
					},
				},
			},
			Name:            naming.PGBackRestRepoContainerName,
			VolumeMounts:    []v1.VolumeMount{sshVolumeMount},
			SecurityContext: initialize.RestrictedSecurityContext(),
			Resources:       resources,
		}

		// Mount PostgreSQL volumes if they are present in the template.
		postgresMounts := map[string]corev1.VolumeMount{
			postgres.DataVolumeMount().Name: postgres.DataVolumeMount(),
			postgres.WALVolumeMount().Name:  postgres.WALVolumeMount(),
		}
		for i := range template.Spec.Volumes {
			if mount, ok := postgresMounts[template.Spec.Volumes[i].Name]; ok {
				container.VolumeMounts = append(container.VolumeMounts, mount)
			}
		}

		template.Spec.Containers = append(template.Spec.Containers, container)
	}

	for _, name := range additionalVolumeMountContainers {
		var containerFound bool
		var index int
		for index = range template.Spec.Containers {
			if template.Spec.Containers[index].Name == name {
				containerFound = true
				break
			}
		}
		if !containerFound {
			return errors.Errorf("Unable to find container %q when adding pgBackRest to Pod",
				name)
		}
		template.Spec.Containers[index].VolumeMounts =
			append(template.Spec.Containers[index].VolumeMounts, sshVolumeMount)
	}

	return nil
}

// ReplicaCreateCommand returns the command that can initialize an instance from
// one of cluster's repositories. It returns nil when no repository is available.
func ReplicaCreateCommand(
	cluster *v1beta1.PostgresCluster, instance *v1beta1.PostgresInstanceSetSpec,
) []string {
	var name string
	if cluster.Status.PGBackRest != nil {
		for _, repo := range cluster.Status.PGBackRest.Repos {
			if repo.ReplicaCreateBackupComplete {
				name = repo.Name
				break
			}
		}
	}
	if name == "" {
		return nil
	}

	return []string{
		"pgbackrest", "restore", "--delta",
		"--stanza=" + DefaultStanzaName,
		"--repo=" + strings.TrimPrefix(name, "repo"),
		"--link-map=pg_wal=" + postgres.WALDirectory(cluster, instance),
	}
}
