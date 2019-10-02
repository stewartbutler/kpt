// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/template"

	"gopkg.in/yaml.v3"
)

type API struct {
	// Metdata contains the Deployment metadata
	Metadata Metadata `yaml:"metadata""`

	Spec Spec `yaml:"spec""`
}

type Spec struct {
	// Replicas is the number of Deployment replicas
	// Defaults to the REPLICAS env var, or 1
	Replicas *int `yaml:"replicas""`
}

type Metadata struct {
	// Name is the Deployment Resource and Container name
	Name string `yaml:"name""`
}

func main() {
	// Parse the configuration and decode it into an object
	d := yaml.NewDecoder(bytes.NewBufferString(os.Getenv("API_CONFIG")))
	d.KnownFields(false)
	api := &API{}
	if err := d.Decode(api); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// copy the input resources so we merge our changes
	if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
		panic(err)
	}
	fmt.Println("\n---")

	// Default the Replicas field
	r := os.Getenv("REPLICAS")
	if r != "" && api.Spec.Replicas == nil {
		replicas, err := strconv.Atoi(r)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		api.Spec.Replicas = &replicas
	}
	if api.Spec.Replicas == nil {
		r := 1
		api.Spec.Replicas = &r
	}

	// Define the template.
	// Disable the duck-commands for this generated Resource so that users don't override
	// the generated values.
	// Execute the template
	t := template.Must(template.New("deployment").Parse(t))
	if err := t.Execute(os.Stdout, api); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

var t = `
apiVersion: v1
kind: Service
metadata:
  # This service is meant to be used by clients of the database. It exposes a ClusterIP that will
  # automatically load balance connections to the different database pods.
  name: {{ .Metadata.Name }}-public
  labels:
    app: {{ .Metadata.Name }}-cockroachdb
  annotations:
    kpt.dev/kio/path: null
    kpt.dev/kio/index: null
spec:
  ports:
  # The main port, served by gRPC, serves Postgres-flavor SQL, internode
  # traffic and the cli.
  - port: 26257
    targetPort: 26257
    name: grpc
  # The secondary port serves the UI as well as health and debug endpoints.
  - port: 8080
    targetPort: 8080
    name: http
  selector:
    app: {{ .Metadata.Name }}-cockroachdb
---
apiVersion: v1
kind: Service
metadata:
  # This service only exists to create DNS entries for each pod in the stateful
  # set such that they can resolve each other's IP addresses. It does not
  # create a load-balanced ClusterIP and should not be used directly by clients
  # in most circumstances.
  name: {{ .Metadata.Name }}
  labels:
    app: {{ .Metadata.Name }}-cockroachdb
  annotations:
    # This is needed to make the peer-finder work properly and to help avoid
    # edge cases where instance 0 comes up after losing its data and needs to
    # decide whether it should create a new cluster or try to join an existing
    # one. If it creates a new cluster when it should have joined an existing
    # one, we'd end up with two separate clusters listening at the same service
    # endpoint, which would be very bad.
    service.alpha.kubernetes.io/tolerate-unready-endpoints: "true"
    # Enable automatic monitoring of all instances when Prometheus is running in the cluster.
    prometheus.io/scrape: "true"
    prometheus.io/path: "_status/vars"
    prometheus.io/port: "8080"
    kpt.dev/kio/path: null
    kpt.dev/kio/index: null
spec:
  ports:
  - port: 26257
    targetPort: 26257
    name: grpc
  - port: 8080
    targetPort: 8080
    name: http
  clusterIP: None
  selector:
    app: {{ .Metadata.Name }}-cockroachdb
---
apiVersion: policy/v1beta1
kind: PodDisruptionBudget
metadata:
  name: cockroachdb-budget
  labels:
    app: {{ .Metadata.Name }}-cockroachdb
  annotations:
    kpt.dev/kio/path: null
    kpt.dev/kio/index: null
spec:
  selector:
    matchLabels:
      app: {{ .Metadata.Name }}-cockroachdb
  minAvailable: 67%
---
apiVersion: apps/v1  #  for k8s versions before 1.9.0 use apps/v1beta2  and before 1.8.0 use extensions/v1beta1
kind: StatefulSet
metadata:
  name: {{ .Metadata.Name }}
  labels:
    app: {{ .Metadata.Name }}-cockroachdb
  annotations:
    kpt.dev/kio/path: null
    kpt.dev/kio/index: null
    kpt.dev/duck/set-image: disabled
    kpt.dev/duck/get-image: disabled
    kpt.dev/duck/set-replicas: disabled
    kpt.dev/duck/get-replicas: disabled
spec:
  serviceName: {{ .Metadata.Name }}
  replicas: {{ .Spec.Replicas }}
  selector:
    matchLabels:
      app: {{ .Metadata.Name }}-cockroachdb
  template:
    metadata:
      labels:
        app: {{ .Metadata.Name }}-cockroachdb
    spec:
      # Init containers are run only once in the lifetime of a pod, before
      # it's started up for the first time. It has to exit successfully
      # before the pod's main containers are allowed to start.
      # This particular init container does a DNS lookup for other pods in
      # the set to help determine whether or not a cluster already exists.
      # If any other pods exist, it creates a file in the cockroach-data
      # directory to pass that information along to the primary container that
      # has to decide what command-line flags to use when starting CockroachDB.
      # This only matters when a pod's persistent volume is empty - if it has
      # data from a previous execution, that data will always be used.
      #
      # If your Kubernetes cluster uses a custom DNS domain, you will have
      # to add an additional arg to this pod: "-domain=<your-custom-domain>"
      initContainers:
      - name: bootstrap
        image: cockroachdb/cockroach-k8s-init:0.1
        imagePullPolicy: IfNotPresent
        args:
        - "-on-start=/on-start.sh"
        - "-service=cockroachdb"
        env:
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        volumeMounts:
        - name: datadir
          mountPath: "/cockroach/cockroach-data"
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
          - weight: 100
            podAffinityTerm:
              labelSelector:
                matchExpressions:
                - key: app
                  operator: In
                  values:
                  - cockroachdb
              topologyKey: kubernetes.io/hostname
      containers:
      - name: cockroachdb
        image: cockroachdb/cockroach:v1.1.0
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 26257
          name: grpc
        - containerPort: 8080
          name: http
        volumeMounts:
        - name: datadir
          mountPath: /cockroach/cockroach-data
        command:
          - "/bin/bash"
          - "-ecx"
          - |
            # The use of qualified ` + "`hostname -f`" + ` is crucial:
            # Other nodes aren't able to look up the unqualified hostname.
            CRARGS=("start" "--logtostderr" "--insecure" "--host" "$(hostname -f)" "--http-host" "0.0.0.0")
            # We only want to initialize a new cluster (by omitting the join flag)
            # if we're sure that we're the first node (i.e. index 0) and that
            # there aren't any other nodes running as part of the cluster that
            # this is supposed to be a part of (which indicates that a cluster
            # already exists and we should make sure not to create a new one).
            # It's fine to run without --join on a restart if there aren't any
            # other nodes.
            if [ ! "$(hostname)" == "cockroachdb-0" ] || \
               [ -e "/cockroach/cockroach-data/cluster_exists_marker" ]
            then
              # We don't join cockroachdb in order to avoid a node attempting
              # to join itself, which currently doesn't work
              # (https://github.com/cockroachdb/cockroach/issues/9625).
              CRARGS+=("--join" "cockroachdb-public")
            fi
            exec /cockroach/cockroach ${CRARGS[*]}
      # No pre-stop hook is required, a SIGTERM plus some time is all that's
      # needed for graceful shutdown of a node.
      terminationGracePeriodSeconds: 60
      volumes:
      - name: datadir
        persistentVolumeClaim:
          claimName: datadir
  volumeClaimTemplates:
  - metadata:
      name: datadir
    spec:
      accessModes:
        - "ReadWriteOnce"
      resources:
        requests:
          storage: 1Gi
`