/*
Copyright 2016 The Kubernetes Authors.

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

package kube

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"time"
)

const (
	inClusterBaseURL = "https://kubernetes"
	maxRetries       = 8
	retryDelay       = 2 * time.Second
)

type Logger interface {
	Printf(s string, v ...interface{})
}

// Client interacts with the Kubernetes api-server.
type Client struct {
	// If Logger is non-nil, log all method calls with it.
	Logger Logger

	baseURL   string
	client    *http.Client
	token     string
	namespace string
	fake      bool
}

func (c *Client) log(methodName string, args ...interface{}) {
	if c.Logger == nil {
		return
	}
	var as []string
	for _, arg := range args {
		as = append(as, fmt.Sprintf("%v", arg))
	}
	c.Logger.Printf("%s(%s)", methodName, strings.Join(as, ", "))
}

type ConflictError error

type request struct {
	method      string
	path        string
	query       map[string]string
	requestBody interface{}
}

func (c *Client) request(r *request, ret interface{}) error {
	out, err := c.requestRetry(r)
	if err != nil {
		return err
	}
	if ret != nil {
		if err := json.Unmarshal(out, ret); err != nil {
			return err
		}
	}
	return nil
}

// Retry on transport failures. Does not retry on 500s.
func (c *Client) requestRetry(r *request) ([]byte, error) {
	if c.fake {
		return []byte("{}"), nil
	}
	var resp *http.Response
	var err error
	backoff := retryDelay
	for retries := 0; retries < maxRetries; retries++ {
		resp, err = c.doRequest(r.method, r.path, r.query, r.requestBody)
		if err == nil {
			break
		}

		time.Sleep(backoff)
		backoff *= 2
	}
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	rb, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 409 {
		return nil, ConflictError(fmt.Errorf("body: %s", string(rb)))
	} else if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("response has status \"%s\" and body \"%s\"", resp.Status, string(rb))
	}
	return rb, nil
}

func (c *Client) doRequest(method, urlPath string, query map[string]string, body interface{}) (*http.Response, error) {
	url := c.baseURL + urlPath
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		buf = bytes.NewBuffer(b)
	}
	req, err := http.NewRequest(method, url, buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if method == http.MethodPatch {
		req.Header.Set("Content-Type", "application/strategic-merge-patch+json")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}

	q := req.URL.Query()
	for k, v := range query {
		q.Add(k, v)
	}
	req.URL.RawQuery = q.Encode()

	return c.client.Do(req)
}

// NewFakeClient creates a client that doesn't do anything.
func NewFakeClient() *Client {
	return &Client{
		namespace: "default",
		fake:      true,
	}
}

// NewClientInCluster creates a Client that works from within a pod.
func NewClientInCluster(namespace string) (*Client, error) {
	tokenFile := "/var/run/secrets/kubernetes.io/serviceaccount/token"
	token, err := ioutil.ReadFile(tokenFile)
	if err != nil {
		return nil, err
	}

	rootCAFile := "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	certData, err := ioutil.ReadFile(rootCAFile)
	if err != nil {
		return nil, err
	}

	cp := x509.NewCertPool()
	cp.AppendCertsFromPEM(certData)

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    cp,
		},
	}
	c := &http.Client{Transport: tr}
	return &Client{
		baseURL:   inClusterBaseURL,
		client:    c,
		token:     string(token),
		namespace: namespace,
	}, nil
}

func labelsToSelector(labels map[string]string) string {
	var sel []string
	for k, v := range labels {
		sel = append(sel, fmt.Sprintf("%s = %s", k, v))
	}
	return strings.Join(sel, ",")
}

func (c *Client) GetPod(name string) (Pod, error) {
	c.log("GetPod", name)
	var retPod Pod
	err := c.request(&request{
		method: http.MethodGet,
		path:   fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", c.namespace, name),
	}, &retPod)
	return retPod, err
}

func (c *Client) ListPods(labels map[string]string) ([]Pod, error) {
	c.log("ListPods", labels)
	var pl struct {
		Items []Pod `json:"items"`
	}
	err := c.request(&request{
		method: http.MethodGet,
		path:   fmt.Sprintf("/api/v1/namespaces/%s/pods", c.namespace),
		query:  map[string]string{"labelSelector": labelsToSelector(labels)},
	}, &pl)
	return pl.Items, err
}

func (c *Client) DeletePod(name string) error {
	c.log("DeletePod", name)
	return c.request(&request{
		method: http.MethodDelete,
		path:   fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", c.namespace, name),
	}, nil)
}

func (c *Client) GetJob(name string) (Job, error) {
	c.log("GetJob", name)
	var retJob Job
	err := c.request(&request{
		method: http.MethodGet,
		path:   fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs/%s", c.namespace, name),
	}, &retJob)
	return retJob, err
}

func (c *Client) ListJobs(labels map[string]string) ([]Job, error) {
	c.log("ListJobs", labels)
	var jl struct {
		Items []Job `json:"items"`
	}
	err := c.request(&request{
		method: http.MethodGet,
		path:   fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs", c.namespace),
		query:  map[string]string{"labelSelector": labelsToSelector(labels)},
	}, &jl)
	return jl.Items, err
}

func (c *Client) CreatePod(p Pod) (Pod, error) {
	c.log("CreatePod", p)
	var retPod Pod
	err := c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("/api/v1/namespaces/%s/pods", c.namespace),
		requestBody: &p,
	}, &retPod)
	return retPod, err
}

func (c *Client) CreateJob(j Job) (Job, error) {
	c.log("CreateJob", j)
	var retJob Job
	err := c.request(&request{
		method:      http.MethodPost,
		path:        fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs", c.namespace),
		requestBody: &j,
	}, &retJob)
	return retJob, err
}

func (c *Client) DeleteJob(name string) error {
	c.log("DeleteJob", name)
	return c.request(&request{
		method: http.MethodDelete,
		path:   fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs/%s", c.namespace, name),
	}, nil)
}

func (c *Client) PatchJob(name string, job Job) (Job, error) {
	c.log("PatchJob", name, job)
	var retJob Job
	err := c.request(&request{
		method:      http.MethodPatch,
		path:        fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs/%s", c.namespace, name),
		requestBody: &job,
	}, &retJob)
	return retJob, err
}

func (c *Client) PatchJobStatus(name string, job Job) (Job, error) {
	c.log("PatchJobStatus", name, job)
	var retJob Job
	err := c.request(&request{
		method:      http.MethodPatch,
		path:        fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs/%s/status", c.namespace, name),
		requestBody: &job,
	}, &retJob)
	return retJob, err
}

func (c *Client) ReplaceSecret(name string, s Secret) error {
	// Ommission of the secret from the logs is purposeful.
	c.log("ReplaceSecret", name)
	return c.request(&request{
		method:      http.MethodPut,
		path:        fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", c.namespace, name),
		requestBody: &s,
	}, nil)
}

func (c *Client) GetLog(pod string) ([]byte, error) {
	c.log("GetLog", pod)
	return c.requestRetry(&request{
		method: http.MethodGet,
		path:   fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log", c.namespace, pod),
	})
}
