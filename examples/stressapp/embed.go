/*
Copyright 2026.

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

// Package stressapp carries the example Riak stress application (riak_stress.py) so
// that the scale-test harness can ship it into a cluster without a container image.
package stressapp

import _ "embed"

// Script is the source of riak_stress.py, a stdlib-only Riak KV load generator.
//
//go:embed riak_stress.py
var Script string
