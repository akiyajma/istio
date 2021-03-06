// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gateway

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	k8s "sigs.k8s.io/service-apis/apis/v1alpha1"

	istio "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/config/schema/resource"
	"istio.io/pkg/log"
)

const (
	ControllerName = "istio.io/gateway-controller"
)

type KubernetesResources struct {
	GatewayClass  []config.Config
	Gateway       []config.Config
	HTTPRoute     []config.Config
	TCPRoute      []config.Config
	TLSRoute      []config.Config
	BackendPolicy []config.Config
	Namespaces    map[string]*corev1.Namespace

	// Domain for the cluster. Typically cluster.local
	Domain string
}

func isRouteMatch(cfg config.Config, res resource.Schema, gatewayNamespace string,
	routes k8s.RouteBindingSelector, namespaces map[string]*corev1.Namespace) bool {
	if routes.Kind != res.Kind() {
		return false
	}
	if routes.Group != "" && routes.Group != res.Group() {
		return false
	}
	ls, err := metav1.LabelSelectorAsSelector(&routes.Selector)
	if err != nil {
		log.Errorf("failed to create route selector: %v", err)
		return false
	}
	if !ls.Matches(klabels.Set(cfg.Labels)) {
		return false
	}

	if routes.Namespaces == nil {
		// "This is restricted to the namespace of this Gateway by default"
		return gatewayNamespace == cfg.Namespace
	}
	switch routes.Namespaces.From {
	case k8s.RouteSelectAll:
		return true
	case k8s.RouteSelectSame:
		if gatewayNamespace != cfg.Namespace {
			return false
		}
	case k8s.RouteSelectSelector:
		ns, err := metav1.LabelSelectorAsSelector(&routes.Namespaces.Selector)
		if err != nil {
			log.Errorf("failed to create namespace selector: %v", err)
			return false
		}
		namespace := namespaces[cfg.Namespace]
		if namespace == nil {
			log.Errorf("missing namespace %v for route %v, skipping", cfg.Namespace, cfg.Name)
			return false
		}
		return ns.Matches(klabels.Set(namespace.Labels))
	}

	return true
}

func (r *KubernetesResources) fetchHTTPRoutes(gatewayNamespace string, routes k8s.RouteBindingSelector) []config.Config {
	result := []config.Config{}
	for _, http := range r.HTTPRoute {
		if isRouteMatch(http, collections.K8SServiceApisV1Alpha1Httproutes.Resource(), gatewayNamespace, routes, r.Namespaces) {
			result = append(result, http)
		}
	}
	return result
}

func (r *KubernetesResources) fetchTCPRoutes(gatewayNamespace string, routes k8s.RouteBindingSelector) []config.Config {
	result := []config.Config{}
	for _, http := range r.TCPRoute {
		if isRouteMatch(http, collections.K8SServiceApisV1Alpha1Tcproutes.Resource(), gatewayNamespace, routes, r.Namespaces) {
			result = append(result, http)
		}
	}
	return result
}

func (r *KubernetesResources) fetchTLSRoutes(gatewayNamespace string, routes k8s.RouteBindingSelector) []config.Config {
	result := []config.Config{}
	for _, http := range r.TLSRoute {
		if isRouteMatch(http, collections.K8SServiceApisV1Alpha1Tlsroutes.Resource(), gatewayNamespace, routes, r.Namespaces) {
			result = append(result, http)
		}
	}
	return result
}

type IstioResources struct {
	Gateway         []config.Config
	VirtualService  []config.Config
	DestinationRule []config.Config
}

var _ = k8s.HTTPRoute{}

func convertResources(r *KubernetesResources) IstioResources {
	result := IstioResources{}
	gw, routeMap := convertGateway(r)
	result.Gateway = gw
	result.VirtualService = convertVirtualService(r, routeMap)
	result.DestinationRule = convertDestinationRule(r)
	return result
}

// Unique key to identify a route
type RouteKey struct {
	Gvk       config.GroupVersionKind
	Name      string
	Namespace string
}

func toRouteKey(c config.Config) RouteKey {
	return RouteKey{
		c.GroupVersionKind,
		c.Name,
		c.Namespace,
	}
}

func convertDestinationRule(r *KubernetesResources) []config.Config {

	result := []config.Config{}
	for _, obj := range r.BackendPolicy {
		bp := obj.Spec.(*k8s.BackendPolicySpec)
		for i, ref := range bp.BackendRefs {
			var serviceName string
			if emptyOrEqual(ref.Group, gvk.Service.CanonicalGroup()) && emptyOrEqual(ref.Kind, gvk.Service.Kind) {
				serviceName = fmt.Sprintf("%s.%s.svc.%s", ref.Name, obj.Namespace, r.Domain)
			} else {
				log.Warnf("unsupported backendRef: %+v", ref)
				continue
			}
			dr := &istio.DestinationRule{
				Host:          serviceName,
				TrafficPolicy: &istio.TrafficPolicy{},
			}
			if bp.TLS != nil && bp.TLS.CertificateAuthorityRef != nil {
				tls := &istio.ClientTLSSettings{
					// Currently, only simple is supported
					CredentialName: buildSecretReference(*bp.TLS.CertificateAuthorityRef),
					Mode:           istio.ClientTLSSettings_SIMPLE,
				}
				if ref.Port != nil {
					dr.TrafficPolicy.PortLevelSettings = append(dr.TrafficPolicy.PortLevelSettings, &istio.TrafficPolicy_PortTrafficPolicy{
						Port: &istio.PortSelector{Number: uint32(*ref.Port)},
						Tls:  tls,
					})
				} else {
					dr.TrafficPolicy.Tls = tls
				}
			}
			drConfig := config.Config{
				Meta: config.Meta{
					CreationTimestamp: obj.CreationTimestamp,
					GroupVersionKind:  gvk.DestinationRule,
					Name:              fmt.Sprintf("%s-%d-%s", obj.Name, i, constants.KubernetesGatewayName),
					Namespace:         obj.Namespace,
					Domain:            r.Domain,
				},
				Spec: dr,
			}
			result = append(result, drConfig)
		}
	}
	return result
}

func convertVirtualService(r *KubernetesResources, routeMap map[RouteKey][]string) []config.Config {
	result := []config.Config{}
	for _, obj := range r.TCPRoute {
		gateways, f := routeMap[toRouteKey(obj)]
		if !f {
			// There are no gateways using this route
			continue
		}

		vsConfig := buildTCPVirtualService(obj, gateways, r.Domain)
		result = append(result, vsConfig)
	}

	for _, obj := range r.TLSRoute {
		gateways, f := routeMap[toRouteKey(obj)]
		if !f {
			// There are no gateways using this route
			continue
		}

		vsConfig := buildTLSVirtualService(obj, gateways, r.Domain)
		result = append(result, vsConfig)
	}

	for _, obj := range r.HTTPRoute {
		gateways, f := routeMap[toRouteKey(obj)]
		if !f {
			// There are no gateways using this route
			continue
		}

		result = append(result, buildHTTPVirtualServices(obj, gateways, r.Domain)...)
	}
	return result
}

func buildHTTPVirtualServices(obj config.Config, gateways []string, domain string) []config.Config {
	result := []config.Config{}

	route := obj.Spec.(*k8s.HTTPRouteSpec)

	name := fmt.Sprintf("%s-%s", obj.Name, constants.KubernetesGatewayName)

	httproutes := []*istio.HTTPRoute{}
	hosts := hostnameToStringList(route.Hostnames)
	for _, r := range route.Rules {
		// TODO: implement redirect, rewrite, timeout, mirror, corspolicy, retries
		vs := &istio.HTTPRoute{}
		for _, match := range r.Matches {
			vs.Match = append(vs.Match, &istio.HTTPMatchRequest{
				Uri:     createURIMatch(match),
				Headers: createHeadersMatch(match),
			})
		}
		for _, filter := range r.Filters {
			switch filter.Type {
			case k8s.HTTPRouteFilterRequestHeaderModifier:
				vs.Headers = createHeadersFilter(filter.RequestHeaderModifier)
			default:
				log.Warnf("unsupported filter type %q", filter.Type)
			}
		}

		vs.Route = buildHTTPDestination(r.ForwardTo, obj.Namespace)
		httproutes = append(httproutes, vs)
	}
	vsConfig := config.Config{
		Meta: config.Meta{
			CreationTimestamp: obj.CreationTimestamp,
			GroupVersionKind:  gvk.VirtualService,
			Name:              name,
			Namespace:         obj.Namespace,
			Domain:            domain,
		},
		Spec: &istio.VirtualService{
			Hosts:    hosts,
			Gateways: gateways,
			Http:     httproutes,
		},
	}
	result = append(result, vsConfig)
	return result
}

func hostnameToStringList(h []k8s.Hostname) []string {
	res := make([]string, 0, len(h))
	for _, i := range h {
		res = append(res, string(i))
	}
	return res
}

func buildTCPVirtualService(obj config.Config, gateways []string, domain string) config.Config {
	route := obj.Spec.(*k8s.TCPRouteSpec)
	routes := []*istio.TCPRoute{}
	for _, r := range route.Rules {
		ir := &istio.TCPRoute{
			Match: buildTCPMatch(r.Matches),
			Route: buildTCPDestination(r.ForwardTo, obj.Namespace),
		}
		routes = append(routes, ir)
	}

	vsConfig := config.Config{
		Meta: config.Meta{
			CreationTimestamp: obj.CreationTimestamp,
			GroupVersionKind:  gvk.VirtualService,
			Name:              fmt.Sprintf("%s-tcp-%s", obj.Name, constants.KubernetesGatewayName),
			Namespace:         obj.Namespace,
			Domain:            domain,
		},
		Spec: &istio.VirtualService{
			// TODO investigate if we should/must constrain this to avoid conflicts
			Hosts:    []string{"*"},
			Gateways: gateways,
			Tcp:      routes,
		},
	}
	return vsConfig
}

func buildTLSVirtualService(obj config.Config, gateways []string, domain string) config.Config {
	route := obj.Spec.(*k8s.TLSRouteSpec)
	routes := []*istio.TLSRoute{}
	for _, r := range route.Rules {
		ir := &istio.TLSRoute{
			Match: buildTLSMatch(r.Matches),
			Route: buildTCPDestination(r.ForwardTo, obj.Namespace),
		}
		routes = append(routes, ir)
	}

	vsConfig := config.Config{
		Meta: config.Meta{
			CreationTimestamp: obj.CreationTimestamp,
			GroupVersionKind:  gvk.VirtualService,
			Name:              fmt.Sprintf("%s-tls-%s", obj.Name, constants.KubernetesGatewayName),
			Namespace:         obj.Namespace,
			Domain:            domain,
		},
		Spec: &istio.VirtualService{
			// TODO investigate if we should/must constrain this to avoid conflicts
			Hosts:    []string{"*"},
			Gateways: gateways,
			Tls:      routes,
		},
	}
	return vsConfig
}

func buildTCPDestination(action []k8s.RouteForwardTo, ns string) []*istio.RouteDestination {
	if len(action) == 0 {
		return nil
	}

	weights := []int{}
	for _, w := range action {
		weights = append(weights, int(w.Weight))
	}
	weights = standardizeWeights(weights)
	res := []*istio.RouteDestination{}
	for i, fwd := range action {
		dst := buildGenericDestination(fwd, ns)
		res = append(res, &istio.RouteDestination{
			Destination: dst,
			Weight:      int32(weights[i]),
		})
	}
	return res
}

func buildTCPMatch([]k8s.TCPRouteMatch) []*istio.L4MatchAttributes {
	// Currently the spec only supports extensions, which are not currently implemented by Istio.
	return nil
}

func buildTLSMatch(match []k8s.TLSRouteMatch) []*istio.TLSMatchAttributes {
	if len(match) == 0 {
		// Istio validation doesn't like empty match, instead do a match all explicitly
		return []*istio.TLSMatchAttributes{{
			SniHosts: []string{"*"},
		}}
	}
	res := make([]*istio.TLSMatchAttributes, 0, len(match))
	for _, m := range match {
		res = append(res, &istio.TLSMatchAttributes{
			SniHosts: hostnamesToStringlist(m.SNIs),
		})
	}
	return res
}

func hostnamesToStringlist(h []k8s.Hostname) []string {
	res := make([]string, 0, len(h))
	for _, i := range h {
		res = append(res, string(i))
	}
	return res
}

func intSum(n []int) int {
	r := 0
	for _, i := range n {
		r += i
	}
	return r
}

func buildHTTPDestination(action []k8s.HTTPRouteForwardTo, ns string) []*istio.HTTPRouteDestination {
	if action == nil {
		return nil
	}

	weights := []int{}
	for _, w := range action {
		weights = append(weights, int(w.Weight))
	}
	weights = standardizeWeights(weights)
	res := []*istio.HTTPRouteDestination{}
	for i, fwd := range action {
		dst := buildDestination(fwd, ns)
		rd := &istio.HTTPRouteDestination{
			Destination: dst,
			Weight:      int32(weights[i]),
		}
		for _, filter := range fwd.Filters {
			switch filter.Type {
			case k8s.HTTPRouteFilterRequestHeaderModifier:
				rd.Headers = createHeadersFilter(filter.RequestHeaderModifier)
			default:
				log.Warnf("unsupported filter type %q", filter.Type)
			}
		}
		res = append(res, rd)
	}
	return res
}

func buildDestination(to k8s.HTTPRouteForwardTo, ns string) *istio.Destination {
	res := &istio.Destination{
		Port: &istio.PortSelector{Number: uint32(to.Port)},
	}
	if to.ServiceName != nil {
		res.Host = fmt.Sprintf("%s.%s.svc.%s", *to.ServiceName, ns, constants.DefaultKubernetesDomain)
	} else if to.BackendRef != nil {
		// TODO support this
		log.Errorf("referencing unsupported destination; backendRef is not supported")
	}
	return res
}

func buildGenericDestination(to k8s.RouteForwardTo, ns string) *istio.Destination {
	res := &istio.Destination{
		Port: &istio.PortSelector{Number: uint32(to.Port)},
	}
	if to.ServiceName != nil {
		res.Host = fmt.Sprintf("%s.%s.svc.%s", *to.ServiceName, ns, constants.DefaultKubernetesDomain)
	} else if to.BackendRef != nil {
		// TODO support this
		log.Errorf("referencing unsupported destination; backendRef is not supported")
	}
	return res
}

// standardizeWeights migrates a list of weights from relative weights, to weights out of 100
// In the event we cannot cleanly move to 100 denominator, we will round up weights in order. See test for details.
// TODO in the future we should probably just make VirtualService support relative weights directly
func standardizeWeights(weights []int) []int {
	if len(weights) == 1 {
		// Instead of setting weight=100 for a single destination, we will not set weight at all
		return []int{0}
	}
	total := intSum(weights)
	if total == 0 {
		// All empty, fallback to even weight
		for i := range weights {
			weights[i] = 1
		}
		total = len(weights)
	}
	results := make([]int, 0, len(weights))
	remainders := make([]float64, 0, len(weights))
	for _, w := range weights {
		perc := float64(w) / float64(total)
		rounded := int(perc * 100)
		remainders = append(remainders, (perc*100)-float64(rounded))
		results = append(results, rounded)
	}
	remaining := 100 - intSum(results)
	order := argsort(remainders)
	for _, idx := range order {
		if remaining <= 0 {
			break
		}
		remaining--
		results[idx]++
	}
	return results
}

type argSlice struct {
	sort.Interface
	idx []int
}

func (s argSlice) Swap(i, j int) {
	s.Interface.Swap(i, j)
	s.idx[i], s.idx[j] = s.idx[j], s.idx[i]
}

func argsort(n []float64) []int {
	s := &argSlice{Interface: sort.Float64Slice(n), idx: make([]int, len(n))}
	for i := range s.idx {
		s.idx[i] = i
	}
	sort.Sort(sort.Reverse(s))
	return s.idx
}

func createHeadersFilter(filter *k8s.HTTPRequestHeaderFilter) *istio.Headers {
	if filter == nil {
		return nil
	}
	return &istio.Headers{
		Request: &istio.Headers_HeaderOperations{
			Add:    filter.Add,
			Remove: filter.Remove,
		},
	}
}

func createHeadersMatch(match k8s.HTTPRouteMatch) map[string]*istio.StringMatch {
	if match.Headers == nil {
		return nil
	}
	res := map[string]*istio.StringMatch{}
	if match.Headers.Type == "" ||
		match.Headers.Type == k8s.HeaderMatchExact ||
		match.Headers.Type == k8s.HeaderMatchImplementationSpecific {
		for k, v := range match.Headers.Values {
			res[k] = &istio.StringMatch{
				MatchType: &istio.StringMatch_Exact{Exact: v},
			}
		}
	} else {
		log.Warnf("unknown type: %v is not supported Header type", match.Headers.Type)
		return nil
	}
	return res
}

func createURIMatch(match k8s.HTTPRouteMatch) *istio.StringMatch {
	if match.Path.Type == "" || match.Path.Type == k8s.PathMatchImplementationSpecific || match.Path.Type == k8s.PathMatchPrefix {
		return &istio.StringMatch{
			MatchType: &istio.StringMatch_Prefix{Prefix: match.Path.Value},
		}
	} else if match.Path.Type == k8s.PathMatchExact {
		return &istio.StringMatch{
			MatchType: &istio.StringMatch_Exact{Exact: match.Path.Value},
		}
	} else if match.Path.Type == k8s.PathMatchRegularExpression {
		return &istio.StringMatch{
			MatchType: &istio.StringMatch_Regex{Regex: match.Path.Value},
		}
	} else {
		log.Warnf("unknown type: %s is not supported Path match type", match.Path.Type)
		return nil
	}
}

// getGatewayClass finds all gateway class that are owned by Istio
func getGatewayClasses(r *KubernetesResources) map[string]struct{} {
	classes := map[string]struct{}{}
	for _, obj := range r.GatewayClass {
		gwc := obj.Spec.(*k8s.GatewayClassSpec)
		if gwc.Controller == ControllerName {
			// TODO we can add any settings we need here needed for the controller
			// For now, we have none, so just add a struct
			classes[obj.Name] = struct{}{}
		}
	}
	return classes
}

func convertGateway(r *KubernetesResources) ([]config.Config, map[RouteKey][]string) {
	result := []config.Config{}
	routeToGateway := map[RouteKey][]string{}
	classes := getGatewayClasses(r)
	for _, obj := range r.Gateway {
		kgw := obj.Spec.(*k8s.GatewaySpec)
		if _, f := classes[kgw.GatewayClassName]; !f {
			// No gateway class found, this may be meant for another controller; should be skipped.
			continue
		}
		name := obj.Name + "-" + constants.KubernetesGatewayName
		var servers []*istio.Server
		for _, l := range kgw.Listeners {
			server := &istio.Server{
				// Allow all hosts here. Specific routing will be determined by the virtual services
				Hosts: buildHostnameMatch(l.Hostname),
				Port: &istio.Port{
					Number: uint32(l.Port),
					// TODO currently we 1:1 support protocols in the API. If this changes we may
					// need more logic here.
					Protocol: string(l.Protocol),
					Name:     fmt.Sprintf("%v-%v-gateway-%s-%s", strings.ToLower(string(l.Protocol)), l.Port, obj.Name, obj.Namespace),
				},
				// TODO support RouteOverride
				Tls: buildTLS(l.TLS),
			}

			servers = append(servers, server)

			// TODO support VirtualService direct reference
			for _, http := range r.fetchHTTPRoutes(obj.Namespace, l.Routes) {
				k := toRouteKey(http)
				routeToGateway[k] = append(routeToGateway[k], obj.Namespace+"/"+name)
			}
			for _, tcp := range r.fetchTCPRoutes(obj.Namespace, l.Routes) {
				k := toRouteKey(tcp)
				routeToGateway[k] = append(routeToGateway[k], obj.Namespace+"/"+name)
			}
			for _, tls := range r.fetchTLSRoutes(obj.Namespace, l.Routes) {
				k := toRouteKey(tls)
				routeToGateway[k] = append(routeToGateway[k], obj.Namespace+"/"+name)
			}
		}
		gatewayConfig := config.Config{
			Meta: config.Meta{
				CreationTimestamp: obj.CreationTimestamp,
				GroupVersionKind:  gvk.Gateway,
				Name:              name,
				Namespace:         obj.Namespace,
				Domain:            r.Domain,
			},
			Spec: &istio.Gateway{
				Servers: servers,
				// TODO derive this from gatewayclass param ref
				Selector: labels.Instance{constants.IstioLabel: "ingressgateway"},
			},
		}
		result = append(result, gatewayConfig)
	}
	return result, routeToGateway
}

func buildTLS(tls *k8s.GatewayTLSConfig) *istio.ServerTLSSettings {
	if tls == nil {
		return nil
	}
	// Explicitly not supported: file mounted
	// Not yet implemented: TLS mode, https redirect, max protocol version, SANs, CipherSuites, VerifyCertificate

	// TODO: "The SNI server_name must match a route host name for the Gateway to route the TLS request."
	// Do we need to do something smarter here to support ^ ?
	out := &istio.ServerTLSSettings{
		HttpsRedirect: false,
	}
	switch tls.Mode {
	case "", k8s.TLSModeTerminate:
		out.Mode = istio.ServerTLSSettings_SIMPLE
		out.CredentialName = buildSecretReference(tls.CertificateRef)
	case k8s.TLSModePassthrough:
		out.Mode = istio.ServerTLSSettings_PASSTHROUGH
	}
	return out
}

func buildSecretReference(ref k8s.LocalObjectReference) string {
	if !emptyOrEqual(ref.Group, gvk.Secret.CanonicalGroup()) || !emptyOrEqual(ref.Kind, gvk.Secret.Kind) {
		log.Errorf("invalid certificate reference %v, only secret is allowed", ref)
		return ""
	}
	return ref.Name
}

func buildHostnameMatch(hostname *k8s.Hostname) []string {
	// service-apis hostname semantics match ours, so pass directly. The one
	// exception is they allow unset, which is equivalent to * for us
	if hostname == nil {
		return []string{"*"}
	}
	return []string{string(*hostname)}
}

func emptyOrEqual(have, expected string) bool {
	return have == "" || have == expected
}
