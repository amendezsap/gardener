// Copyright (c) 2019 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
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

package storage

import (
	"context"

	"github.com/gardener/gardener/pkg/apis/core"
	"k8s.io/apimachinery/pkg/api/meta"
	metatable "k8s.io/apimachinery/pkg/api/meta/table"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1beta1 "k8s.io/apimachinery/pkg/apis/meta/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"
)

var swaggerMetadataDescriptions = metav1.ObjectMeta{}.SwaggerDoc()

type convertor struct {
	headers []metav1beta1.TableColumnDefinition
}

func newTableConvertor() rest.TableConvertor {
	return &convertor{
		headers: []metav1beta1.TableColumnDefinition{
			{Name: "Name", Type: "string", Format: "name", Description: swaggerMetadataDescriptions["name"]},
			{Name: "Provider", Type: "string", Format: "name", Description: swaggerMetadataDescriptions["provider"]},
			{Name: "Seed", Type: "string", Format: "name", Description: swaggerMetadataDescriptions["seed"]},
			{Name: "Operation", Type: "string", Format: "name", Description: swaggerMetadataDescriptions["operation"]},
			{Name: "Progress", Type: "string", Format: "name", Description: swaggerMetadataDescriptions["progress"]},
			{Name: "Age", Type: "date", Description: swaggerMetadataDescriptions["creationTimestamp"]},
		},
	}
}

// ConvertToTable converts the output to a table.
func (c *convertor) ConvertToTable(ctx context.Context, obj runtime.Object, tableOptions runtime.Object) (*metav1beta1.Table, error) {
	var (
		err   error
		table = &metav1beta1.Table{
			ColumnDefinitions: c.headers,
		}
	)

	if m, err := meta.ListAccessor(obj); err == nil {
		table.ResourceVersion = m.GetResourceVersion()
		table.Continue = m.GetContinue()
	} else {
		if m, err := meta.CommonAccessor(obj); err == nil {
			table.ResourceVersion = m.GetResourceVersion()
		}
	}

	table.Rows, err = metatable.MetaToTableRow(obj, func(obj runtime.Object, m metav1.Object, name, age string) ([]interface{}, error) {
		var (
			backupBucket = obj.(*core.BackupBucket)
			cells        = []interface{}{}
		)

		cells = append(cells, backupBucket.Name)
		cells = append(cells, backupBucket.Spec.Provider.Type)
		cells = append(cells, backupBucket.Spec.SeedName)
		if lastOp := backupBucket.Status.LastOperation; lastOp != nil {
			cells = append(cells, lastOp.State)
			cells = append(cells, lastOp.Progress)
		} else {
			cells = append(cells, "<pending>")
			cells = append(cells, 0)
		}
		cells = append(cells, metatable.ConvertToHumanReadableDateType(backupBucket.CreationTimestamp))

		return cells, nil
	})

	return table, err
}
