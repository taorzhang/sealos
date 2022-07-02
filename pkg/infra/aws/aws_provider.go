// Copyright © 2021 Awsbaba Group Holding Ltd.
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

package aws

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/labring/sealos/pkg/utils/rand"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"strings"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/labring/sealos/pkg/types/v1beta1"
	"github.com/labring/sealos/pkg/utils/logger"
)

type ActionName string

const (
	CreateVPC           ActionName = "CreateVPC"
	CreateSecurityGroup ActionName = "CreateSecurityGroup"
	ReconcileInstance   ActionName = "ReconcileInstance"
	BindEIP             ActionName = "BindEIP"
	ReleaseEIP          ActionName = "ReleaseEIP"
	ClearInstances      ActionName = "ClearInstances"
	DeleteVSwitch       ActionName = "DeleteVSwitch"
	DeleteSecurityGroup ActionName = "DeleteSecurityGroup"
	DeleteGateway       ActionName = "DeleteGateway"
	DeleteSubnets       ActionName = "DeleteSubnets"
	DeleteVPC           ActionName = "DeleteVPC"
	GetZoneID           ActionName = "GetAvailableZoneID"
)

type AwsProvider struct {
	EC2Helper EC2Helper
	Infra     *v1beta1.Infra
}

func NewEc2Helper(sess *session.Session) *EC2Helper {
	return &EC2Helper{
		Svc:  ec2.New(sess),
		Sess: sess,
	}
}

func (a *AwsProvider) NewClient() error {
	if len(a.Infra.Spec.Metadata.RegionIDs) == 0 {
		return errors.New("your infra module not set region id")
	}
	if len(a.Infra.Spec.Credential.AccessKey) == 0 {
		return errors.New("your infra module not set AccessKey")
	}
	if len(a.Infra.Spec.Credential.AccessSecret) == 0 {
		return errors.New("your infra module not set AccessSecret")
	}

	regionID := a.Infra.Spec.Metadata.RegionIDs[rand.Rand(len(a.Infra.Spec.Metadata.RegionIDs))]
	a.Infra.Status.Cluster.RegionID = regionID
	logger.Info("using regionID is %s", regionID)
	// Start a new session, with the default credentials and config loading
	sess := session.Must(
		session.NewSessionWithOptions(session.Options{
			Config: aws.Config{
				Credentials: credentials.NewStaticCredentials(a.Infra.Spec.Credential.AccessKey, a.Infra.Spec.Credential.AccessSecret, ""),
				Region:      aws.String(regionID),
			},
			SharedConfigState: session.SharedConfigEnable,
		}))

	h := NewEc2Helper(sess)
	a.EC2Helper = *h
	return nil
}

type AwsFunc func() error

func (a *AwsProvider) ReconcileResource(resourceKey ResourceName, action AwsFunc) error {
	if resourceKey.Value(a.Infra.Status) != "" {
		logger.Debug("using resource status value %s: %s", resourceKey, resourceKey.Value(a.Infra.Status))
		return nil
	}
	if err := action(); err != nil {
		logger.Error("reconcile resource %s failed err: %s", resourceKey, err)
		return err
	}
	if resourceKey.Value(a.Infra.Status) != "" {
		logger.Info("create resource success %s: %s", resourceKey, resourceKey.Value(a.Infra.Status))
	}
	return nil
}

func (a *AwsProvider) RetryEcsInstanceType(req *ec2.RunInstancesInput) ([]*ec2.Instance, error) {
	instances, err := a.EC2Helper.Svc.RunInstances(req)
	if err != nil {
		return nil, err
	}
	if len(instances.Instances) == 0 {
		return nil, errors.New("Run instances failed.")
	}
	return instances.Instances, nil
}

func (a *AwsProvider) DeleteResource(resourceKey ResourceName, action AwsFunc) {
	val := resourceKey.Value(a.Infra.Status)
	if val == "" {
		logger.Warn("delete resource not exists %s", resourceKey)
		return
	}
	if err := action(); err != nil {
		logger.Error("delete resource %s failed err: %s", resourceKey, err)
	} else {
		logger.Info("delete resource Success %s: %s", resourceKey, val)
	}
}

var RecocileFuncMap = map[ActionName]func(provider *AwsProvider) error{
	CreateVPC: func(AwsProvider *AwsProvider) error {
		return AwsProvider.ReconcileResource(VpcID, AwsProvider.CreateVPC)
	},
	CreateSecurityGroup: func(AwsProvider *AwsProvider) error {
		return AwsProvider.ReconcileResource(SecurityGroupID, AwsProvider.CreateSecurityGroup)
	},
	ReconcileInstance: func(AwsProvider *AwsProvider) error {
		var errorMsg []string
		current := sets.NewString()
		spec := sets.NewString()
		for _, h := range AwsProvider.Infra.Status.Hosts {
			current.Insert(strings.Join(h.Roles, ","))
		}
		for _, h := range AwsProvider.Infra.Spec.Hosts {
			spec.Insert(strings.Join(h.Roles, ","))
			host := &h
			statusIndex := AwsProvider.Infra.Status.FindHostsByRoles(h.Roles)
			if statusIndex < 0 {
				AwsProvider.Infra.Status.Hosts = append(AwsProvider.Infra.Status.Hosts, v1beta1.InfraHostStatus{Roles: h.Roles})
				statusIndex = len(AwsProvider.Infra.Status.Hosts) - 1
			}
			status := &AwsProvider.Infra.Status.Hosts[statusIndex]
			err := AwsProvider.ReconcileInstances(host, status)
			if err != nil {
				errorMsg = append(errorMsg, err.Error())
				status.Ready = false
			} else {
				status.Ready = true
			}
		}
		deleteData := current.Difference(spec)
		var instanceIDs []string
		finalStatus := AwsProvider.Infra.Status.Hosts
		for _, roles := range deleteData.List() {
			statusIndex := AwsProvider.Infra.Status.FindHostsByRolesString(roles)
			ids := AwsProvider.Infra.Status.Hosts[statusIndex].IDs
			instanceIDs = append(instanceIDs, ids)
			finalStatus = append(finalStatus[:statusIndex], finalStatus[statusIndex+1:]...)
		}
		if len(instanceIDs) != 0 {
			ShouldBeDeleteInstancesIDs.SetValue(AwsProvider.Infra.Status, strings.Join(instanceIDs, ","))
			AwsProvider.DeleteResource(ShouldBeDeleteInstancesIDs, AwsProvider.DeleteInstances)
			AwsProvider.Infra.Status.Hosts = finalStatus
		}

		if len(errorMsg) == 0 {
			return nil
		}
		return errors.New(strings.Join(errorMsg, " && "))
	},
	GetZoneID: func(AwsProvider *AwsProvider) error {
		return AwsProvider.ReconcileResource(ZoneID, AwsProvider.GetAvailableZoneID)
	},
	BindEIP: func(AwsProvider *AwsProvider) error {
		return AwsProvider.ReconcileResource(EipID, AwsProvider.BindEipForMaster0)
	},
}

var DeleteFuncMap = map[ActionName]func(provider *AwsProvider){
	ClearInstances: func(AwsProvider *AwsProvider) {
		var instanceIDs []string
		for _, h := range AwsProvider.Infra.Status.Hosts {
			instances, err := AwsProvider.GetInstancesInfo(h.ToHost(), JustGetInstanceInfo)
			if err != nil {
				logger.Error("get %s instanceInfo failed %v", strings.Join(h.Roles, ","), err)
			}
			for _, instance := range instances {
				instanceIDs = append(instanceIDs, instance.InstanceID)
			}
			logger.Debug("instance ids %v", instanceIDs)
		}

		if len(instanceIDs) != 0 {
			ShouldBeDeleteInstancesIDs.SetValue(AwsProvider.Infra.Status, strings.Join(instanceIDs, ","))
		}
		AwsProvider.DeleteResource(ShouldBeDeleteInstancesIDs, AwsProvider.DeleteInstances)

		// wait instance resource deleted.
	},
	DeleteSubnets: func(provider *AwsProvider) {
		provider.DeleteResource(SecurityGroupID, provider.DeleteSubnets)
	},
	DeleteSecurityGroup: func(provider *AwsProvider) {
		provider.DeleteResource(SecurityGroupID, provider.DeleteSecurityGroup)
	},
	DeleteGateway: func(provider *AwsProvider) {
		provider.DeleteResource(SecurityGroupID, provider.DeleteGateway)
	},
	DeleteVPC: func(AwsProvider *AwsProvider) {
		AwsProvider.DeleteResource(VpcID, AwsProvider.DeleteVPC)
	},
}

func (a *AwsProvider) ClearCluster() {
	todolist := []ActionName{
		//ReleaseEIP,
		ClearInstances,
		DeleteSubnets,
		DeleteGateway,
		DeleteSecurityGroup,
		DeleteVPC,
	}
	for _, name := range todolist {
		DeleteFuncMap[name](a)
	}
}

func (a *AwsProvider) Reconcile() error {
	if a.Infra.DeletionTimestamp != nil {
		logger.Info("DeletionTimestamp not nil Clear Infra")
		a.ClearCluster()
		return nil
	}
	todolist := []ActionName{
		GetZoneID,
		CreateVPC,
		CreateSecurityGroup,
		ReconcileInstance,
		// BindEIP,
	}

	for _, actionName := range todolist {
		err := RecocileFuncMap[actionName](a)
		if err != nil {
			logger.Warn("actionName: %s,err: %v ,skip it", actionName, err)
			return err
		}
	}

	return nil
}

func (a *AwsProvider) Apply() (err error) {
	if err = v1beta1.DefaultInfra(a.Infra, DefaultInfra); err != nil {
		return err
	}
	//if err := DefaultValidate(a.Infra, DefaultInfra); len(err) != 0 {
	//	return err.ToAggregate()
	//}
	// a.ClearCluster()
	err = a.Reconcile()
	return err
}

func DefaultInfra(infra *v1beta1.Infra) error {
	//https://help.Awsyun.com/document_detail/63440.htm?spm=a2c4g.11186623.0.0.f5952752gkxpB7#t9856.html
	if infra.Spec.Metadata.Instance.IsSeize {
		infra.Status.Cluster.SpotStrategy = "SpotAsPriceGo"
	} else {
		infra.Status.Cluster.SpotStrategy = "NoSpot"
	}
	return nil
}

func DefaultValidate(infra *v1beta1.Infra) field.ErrorList {
	allErrors := field.ErrorList{}
	return allErrors
}
