//go:build wasm_unknown

package main

import sdk "github.com/aiii-dot-id/aii-plugin-sdk/pkg/aiiosdk"

func init() { newPlanningPlugin().Run() }

func main() { sdk.MainDescribe() }
