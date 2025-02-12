/*
Copyright 2022 The Koordinator Authors.

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

package core

import (
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	quotav1 "k8s.io/apiserver/pkg/quota/v1"
	"sigs.k8s.io/scheduler-plugins/pkg/apis/scheduling/v1alpha1"

	"github.com/koordinator-sh/koordinator/apis/extension"
)

type QuotaCalculateInfo struct {
	// The semantics of "max" is the quota group's upper limit of resources.
	Max v1.ResourceList `json:"max,omitempty"`
	// The semantics of "min" is the quota group's guaranteed resources, if quota group's "request" less than or
	// equal to "min", the quota group can obtain equivalent resources to the "request"
	OriginalMin v1.ResourceList `json:"originalMin,omitempty"`
	// If Child's sumMin is larger than totalResource, the value of OriginalMin should be scaled in equal proportion
	//to ensure the correctness and fairness of min
	AutoScaleMin v1.ResourceList `json:"autoScaleMin,omitempty"`
	// All assigned pods used
	Used v1.ResourceList `json:"used,omitempty"`
	// All pods request
	Request v1.ResourceList `json:"request,omitempty"`
	// SharedWeight determines the ability of quota groups to compete for shared resources
	SharedWeight v1.ResourceList `json:"sharedWeight,omitempty"`
	// Runtime is the current actual resource that can be used by the quota group
	Runtime v1.ResourceList `json:"runtime,omitempty"`
}

type QuotaInfo struct {
	// QuotaName
	Name string `json:"name,omitempty"`
	// Quota's ParentName
	ParentName string `json:"parentName,omitempty"`
	//IsParent quota group
	IsParent bool `json:"isParent"`
	// If runtimeVersion not equal to quotaTree runtimeVersion, means runtime has been updated.
	RuntimeVersion int64 `json:"runtimeVersion"`
	// Allow lent resource to other quota group
	AllowLentResource bool               `json:"allowLentResource"`
	CalculateInfo     QuotaCalculateInfo `json:"calculateInfo,omitempty"`
	lock              sync.Mutex
}

func NewQuotaInfo(isParent, allowLentResource bool, name, parentName string) *QuotaInfo {
	return &QuotaInfo{
		Name:              name,
		ParentName:        parentName,
		IsParent:          isParent,
		AllowLentResource: allowLentResource,
		RuntimeVersion:    0,
		CalculateInfo: QuotaCalculateInfo{
			Max:          v1.ResourceList{},
			AutoScaleMin: v1.ResourceList{},
			OriginalMin:  v1.ResourceList{},
			Used:         v1.ResourceList{},
			Request:      v1.ResourceList{},
			SharedWeight: v1.ResourceList{},
			Runtime:      v1.ResourceList{},
		},
	}
}
func (qi *QuotaInfo) DeepCopy() *QuotaInfo {
	if qi == nil {
		return nil
	}
	qi.lock.Lock()
	defer qi.lock.Unlock()

	return &QuotaInfo{
		Name:              qi.Name,
		ParentName:        qi.ParentName,
		IsParent:          qi.IsParent,
		AllowLentResource: qi.AllowLentResource,
		RuntimeVersion:    qi.RuntimeVersion,
		CalculateInfo: QuotaCalculateInfo{
			Max:          qi.CalculateInfo.Max.DeepCopy(),
			AutoScaleMin: qi.CalculateInfo.AutoScaleMin.DeepCopy(),
			OriginalMin:  qi.CalculateInfo.OriginalMin.DeepCopy(),
			Used:         qi.CalculateInfo.Used.DeepCopy(),
			Request:      qi.CalculateInfo.Request.DeepCopy(),
			SharedWeight: qi.CalculateInfo.SharedWeight.DeepCopy(),
			Runtime:      qi.CalculateInfo.Runtime.DeepCopy(),
		},
	}
}

// UpdateQuotaInfoFromRemote the CRD(max/oriMin/sharedWeight/allowLentResource/isParent/ParentName) of the quota maybe changed,
// so need update localQuotaInfo's information from inputQuotaInfo.
func (qi *QuotaInfo) UpdateQuotaInfoFromRemote(quotaInfo *QuotaInfo) {
	qi.lock.Lock()
	defer qi.lock.Unlock()

	if quotaInfo.Name == extension.SystemQuotaName {
		return
	}

	qi.setMaxQuotaNoLock(quotaInfo.CalculateInfo.Max)
	qi.setOriginalMinQuotaNoLock(quotaInfo.CalculateInfo.OriginalMin)
	sharedWeight := quotaInfo.CalculateInfo.SharedWeight.DeepCopy()
	if quotav1.IsZero(sharedWeight) {
		sharedWeight = quotaInfo.CalculateInfo.Max.DeepCopy()
	}
	qi.CalculateInfo.SharedWeight = sharedWeight
	qi.AllowLentResource = quotaInfo.AllowLentResource
	qi.IsParent = quotaInfo.IsParent
	qi.ParentName = quotaInfo.ParentName
}

// getLimitRequestNoLock returns the min value of request and max, as max is the quotaGroup's upper limit of resources.
// As the multi-hierarchy quota Model described in the PR, when passing a request upwards, passing a request exceeding its
// max will result in a wrong/invalid runtime distribution. For example, parentQuotaGroup's Max is 20, childGroup's Max
// is 10, and the childGroup's request is 30. If the child passes 30 request upwards and get a 20 runtime back
//(limited by the parent's max is 20), the child can only use 10 (limited by its max).
func (qi *QuotaInfo) getLimitRequestNoLock() v1.ResourceList {
	limitRequest := qi.CalculateInfo.Request.DeepCopy()
	for resName, quantity := range limitRequest {
		if maxQuantity, ok := qi.CalculateInfo.Max[resName]; ok {
			if quantity.Cmp(maxQuantity) == 1 {
				//req > max, limitRequest = max
				limitRequest[resName] = maxQuantity.DeepCopy()
			}
		}
	}
	return limitRequest
}

func (qi *QuotaInfo) addRequestNonNegativeNoLock(delta v1.ResourceList) {
	qi.CalculateInfo.Request = quotav1.Add(qi.CalculateInfo.Request, delta)
	for _, resName := range quotav1.IsNegative(qi.CalculateInfo.Request) {
		qi.CalculateInfo.Request[resName] = *resource.NewQuantity(0, resource.DecimalSI)
	}
}

func (qi *QuotaInfo) addUsedNonNegativeNoLock(delta v1.ResourceList) {
	qi.CalculateInfo.Used = quotav1.Add(qi.CalculateInfo.Used, delta)
	for _, resName := range quotav1.IsNegative(qi.CalculateInfo.Used) {
		qi.CalculateInfo.Used[resName] = *resource.NewQuantity(0, resource.DecimalSI)
	}
}

func (qi *QuotaInfo) setMaxQuotaNoLock(res v1.ResourceList) {
	qi.CalculateInfo.Max = res.DeepCopy()
}

func (qi *QuotaInfo) setOriginalMinQuotaNoLock(res v1.ResourceList) {
	qi.CalculateInfo.OriginalMin = res.DeepCopy()
}

func (qi *QuotaInfo) setAutoScaleMinQuotaNoLock(res v1.ResourceList) {
	qi.CalculateInfo.AutoScaleMin = res.DeepCopy()
}

func (qi *QuotaInfo) setSharedWeightNoLock(res v1.ResourceList) {
	qi.CalculateInfo.SharedWeight = res.DeepCopy()
}

func (qi *QuotaInfo) GetRequest() v1.ResourceList {
	qi.lock.Lock()
	defer qi.lock.Unlock()
	return qi.CalculateInfo.Request.DeepCopy()
}

func (qi *QuotaInfo) GetUsed() v1.ResourceList {
	qi.lock.Lock()
	defer qi.lock.Unlock()
	return qi.CalculateInfo.Used.DeepCopy()
}

func (qi *QuotaInfo) GetRuntime() v1.ResourceList {
	qi.lock.Lock()
	defer qi.lock.Unlock()
	return qi.CalculateInfo.Runtime.DeepCopy()
}

func (qi *QuotaInfo) GetMax() v1.ResourceList {
	qi.lock.Lock()
	defer qi.lock.Unlock()
	return qi.CalculateInfo.Max.DeepCopy()
}

func NewQuotaInfoFromQuota(quota *v1alpha1.ElasticQuota) *QuotaInfo {
	isParent := extension.IsParentQuota(quota)
	parentName := extension.GetParentQuotaName(quota)

	allowLentResource := extension.IsAllowLentResource(quota)

	quotaInfo := NewQuotaInfo(isParent, allowLentResource, quota.Name, parentName)
	quotaInfo.setOriginalMinQuotaNoLock(quota.Spec.Min)
	quotaInfo.setMaxQuotaNoLock(quota.Spec.Max)
	newSharedWeight := extension.GetSharedWeight(quota)
	quotaInfo.setSharedWeightNoLock(newSharedWeight)

	return quotaInfo
}

func (qi *QuotaInfo) getMaskedRuntimeNoLock() v1.ResourceList {
	return quotav1.Mask(qi.CalculateInfo.Runtime, quotav1.ResourceNames(qi.CalculateInfo.Max))
}

func (qi *QuotaInfo) clearForResetNoLock() {
	qi.CalculateInfo.Request = v1.ResourceList{}
	qi.CalculateInfo.Used = v1.ResourceList{}
	qi.CalculateInfo.Runtime = v1.ResourceList{}
	qi.RuntimeVersion = 0
}

// QuotaTopoNode only contains the topology of the parent/child relationship,
// helps to reconstruct quotaTree from the rootQuotaGroup to all the leafQuotaNode.
type QuotaTopoNode struct {
	name                 string
	quotaInfo            *QuotaInfo
	parQuotaTopoNode     *QuotaTopoNode
	childGroupQuotaInfos map[string]*QuotaTopoNode
}

func NewQuotaTopoNode(quotaInfo *QuotaInfo) *QuotaTopoNode {
	return &QuotaTopoNode{
		name:                 quotaInfo.Name,
		quotaInfo:            quotaInfo, // not deepCopy
		childGroupQuotaInfos: make(map[string]*QuotaTopoNode),
	}
}

func (qtn *QuotaTopoNode) AddChildGroupQuotaInfo(childNode *QuotaTopoNode) {
	qtn.childGroupQuotaInfos[childNode.name] = childNode
}

func (qtn *QuotaTopoNode) GetChildGroupQuotaInfos() map[string]*QuotaTopoNode {
	group := make(map[string]*QuotaTopoNode)
	for key, v := range qtn.childGroupQuotaInfos {
		group[key] = v
	}
	return group
}
