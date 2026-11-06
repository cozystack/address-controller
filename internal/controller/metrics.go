/*
Copyright 2026 The Cozystack Authors.

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

package controller

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	localv1alpha1 "github.com/cozystack/address-controller/api/v1alpha1"
)

var (
	addressesDesc = prometheus.NewDesc(
		"address_controller_ipaddresses",
		"IPAddress objects by class and phase.",
		[]string{"class", "phase"}, nil)
	claimsDesc = prometheus.NewDesc(
		"address_controller_ipaddressclaims",
		"IPAddressClaim objects by namespace, resolved class and phase.",
		[]string{"namespace", "class", "phase"}, nil)
)

// InventoryCollector reports the ledger as gauges, counted from the cache
// at scrape time, so the counts hold no state of their own.
type InventoryCollector struct {
	Reader client.Reader
}

// Describe implements prometheus.Collector.
func (c *InventoryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- addressesDesc
	ch <- claimsDesc
}

// Collect implements prometheus.Collector. A failed list reports an
// invalid metric, which fails the scrape visibly instead of reporting zero.
func (c *InventoryCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	addrs := &localv1alpha1.IPAddressList{}
	if err := c.Reader.List(ctx, addrs); err != nil {
		ch <- prometheus.NewInvalidMetric(addressesDesc, err)
	} else {
		counts := map[[2]string]int{}
		for _, a := range addrs.Items {
			counts[[2]string{a.Spec.ClassName, string(a.Status.Phase)}]++
		}
		for k, n := range counts {
			ch <- prometheus.MustNewConstMetric(addressesDesc, prometheus.GaugeValue, float64(n), k[0], k[1])
		}
	}

	claims := &localv1alpha1.IPAddressClaimList{}
	if err := c.Reader.List(ctx, claims); err != nil {
		ch <- prometheus.NewInvalidMetric(claimsDesc, err)
		return
	}
	counts := map[[3]string]int{}
	for _, cl := range claims.Items {
		counts[[3]string{cl.Namespace, cl.Status.ClassName, string(cl.Status.Phase)}]++
	}
	for k, n := range counts {
		ch <- prometheus.MustNewConstMetric(claimsDesc, prometheus.GaugeValue, float64(n), k[0], k[1], k[2])
	}
}
