// Copyright 2016-2018, Pulumi Corporation.
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

package deploy

import (
	"github.com/pkg/errors"

	"github.com/pulumi/pulumi/pkg/diag"
	"github.com/pulumi/pulumi/pkg/resource"
	"github.com/pulumi/pulumi/pkg/resource/plugin"
	"github.com/pulumi/pulumi/pkg/tokens"
	"github.com/pulumi/pulumi/pkg/util/contract"
	"github.com/pulumi/pulumi/pkg/util/logging"
)

// stepGenerator is responsible for turning resource events into steps that
// can be fed to the plan executor. It does this by consulting the plan
// and calculating the appropriate step action based on the requested goal
// state and the existing state of the world.
type stepGenerator struct {
	plan *Plan   // the plan to which this step generator belongs
	opts Options // options for this step generator

	urns     map[resource.URN]bool // set of URNs discovered for this plan
	deletes  map[resource.URN]bool // set of URNs deleted in this plan
	replaces map[resource.URN]bool // set of URNs replaced in this plan
	updates  map[resource.URN]bool // set of URNs updated in this plan
	creates  map[resource.URN]bool // set of URNs created in this plan
	sames    map[resource.URN]bool // set of URNs that were not changed in this plan
}

// GenerateSteps produces one or more steps required to achieve the goal state
// specified by the incoming RegisterResourceEvent.
//
// If the given resource is a custom resource, the step generator will invoke Diff
// and Check on the provider associated with that resource. If those fail, an error
// is returned.
func (sg *stepGenerator) GenerateSteps(event RegisterResourceEvent) ([]Step, error) {
	var invalid bool // will be set to true if this object fails validation.

	goal := event.Goal()
	// generate an URN for this new resource.
	urn := sg.generateURN(event)
	if sg.urns[urn] {
		invalid = true
		// TODO[pulumi/pulumi-framework#19]: improve this error message!
		sg.plan.Diag().Errorf(diag.GetDuplicateResourceURNError(urn), urn)
	}

	// Check for an old resource so that we can figure out if this is a create, delete, etc., and/or to diff.
	old, hasOld := sg.plan.Olds()[urn]
	var oldInputs resource.PropertyMap
	var oldOutputs resource.PropertyMap
	if hasOld {
		oldInputs = old.Inputs
		oldOutputs = old.Outputs
	}

	// Produce a new state object that we'll build up as operations are performed.  Ultimately, this is what will
	// get serialized into the checkpoint file.  Normally there are no outputs, unless this is a refresh.
	props, inputs, outputs, new := sg.getResourcePropertyStates(urn, goal)

	// Fetch the provider for this resource type, assuming it isn't just a logical one.
	var prov plugin.Provider
	var err error
	if goal.Custom {
		if prov, err = sg.provider(goal.Type); err != nil {
			return nil, err
		}
	}

	// See if we're performing a refresh update, which takes slightly different code-paths.
	refresh := sg.plan.IsRefresh()

	// We only allow unknown property values to be exposed to the provider if we are performing an update preview.
	allowUnknowns := sg.plan.preview && !refresh

	// We may be re-creating this resource if it got deleted earlier in the execution of this plan.
	_, recreating := sg.deletes[urn]

	// If this isn't a refresh, ensure the provider is okay with this resource and fetch the inputs to pass to
	// subsequent methods.  If these are not inputs, we are just going to blindly store the outputs, so skip this.
	if prov != nil && !refresh {
		var failures []plugin.CheckFailure

		// If we are re-creating this resource because it was deleted earlier, the old inputs are now
		// invalid (they got deleted) so don't consider them.
		if recreating {
			inputs, failures, err = prov.Check(urn, nil, goal.Properties, allowUnknowns)
		} else {
			inputs, failures, err = prov.Check(urn, oldInputs, inputs, allowUnknowns)
		}

		if err != nil {
			return nil, err
		} else if sg.issueCheckErrors(new, urn, failures) {
			invalid = true
		}
		props = inputs
		new.Inputs = inputs
	}

	// Next, give each analyzer -- if any -- a chance to inspect the resource too.
	for _, a := range sg.plan.analyzers {
		var analyzer plugin.Analyzer
		analyzer, err = sg.plan.ctx.Host.Analyzer(a)
		if err != nil {
			return nil, err
		} else if analyzer == nil {
			return nil, errors.Errorf("analyzer '%v' could not be loaded from your $PATH", a)
		}
		var failures []plugin.AnalyzeFailure
		failures, err = analyzer.Analyze(new.Type, props)
		if err != nil {
			return nil, err
		}
		for _, failure := range failures {
			invalid = true
			sg.plan.Diag().Errorf(
				diag.GetAnalyzeResourceFailureError(urn), a, urn, failure.Property, failure.Reason)
		}
	}

	// If the resource isn't valid, don't proceed any further.
	if invalid {
		return nil, errors.New("One or more resource validation errors occurred; refusing to proceed")
	}

	// There are three cases we need to consider when figuring out what to do with this resource.
	//
	// Case 1: recreating
	//  In this case, we have seen a resource with this URN before and we have already issued a
	//  delete step for it. This happens when the engine has to delete a resource before it has
	//  enough information about whether that resource still exists. A concrete example is
	//  when a resource depends on a resource that is delete-before-replace: the engine must first
	//  delete the dependent resource before depending the DBR resource, but the engine can't know
	//  yet whether the dependent resource is being replaced or deleted.
	//
	//  In this case, we are seeing the resource again after deleting it, so it must be a replacement.
	//
	//  Logically, recreating implies hasOld, since in order to delete something it must have
	//  already existed.
	contract.Assert(!recreating || hasOld)
	if recreating {
		logging.V(7).Infof("Planner decided to re-create replaced resource '%v' deleted due to dependent DBR", urn)
		contract.Assert(!refresh)

		// Unmark this resource as deleted, we now know it's being replaced instead.
		delete(sg.deletes, urn)
		sg.replaces[urn] = true
		return []Step{
			NewReplaceStep(sg.plan, old, new, nil, false),
			NewCreateReplacementStep(sg.plan, event, old, new, nil, false),
		}, nil
	}

	// Case 2: hasOld
	//  In this case, the resource we are operating upon now exists in the old snapshot.
	//  It must be an update or a replace. Which operation we do depends on the provider's
	//  response to `Diff`. We must:
	//    - Check whether the update requires replacement (`Diff`)
	//    - If yes, create a new copy, and mark it as having been replaced.
	//    - If no, simply update the existing resource in place.
	if hasOld {
		contract.Assert(old != nil && old.Type == new.Type)

		// Determine whether the change resulted in a diff.
		diff, err := sg.diff(urn, old.ID, oldInputs, oldOutputs, inputs, outputs, props, prov, refresh,
			allowUnknowns)
		if err != nil {
			return nil, err
		}

		// Ensure that we received a sensible response.
		if diff.Changes != plugin.DiffNone && diff.Changes != plugin.DiffSome {
			return nil, errors.Errorf(
				"unrecognized diff state for %s: %d", urn, diff.Changes)
		}

		// If there were changes, check for a replacement vs. an in-place update.
		if diff.Changes == plugin.DiffSome {
			if diff.Replace() {
				sg.replaces[urn] = true

				// If we are going to perform a replacement, we need to recompute the default values.  The above logic
				// had assumed that we were going to carry them over from the old resource, which is no longer true.
				if prov != nil && !refresh {
					var failures []plugin.CheckFailure
					inputs, failures, err = prov.Check(urn, nil, goal.Properties, allowUnknowns)
					if err != nil {
						return nil, err
					} else if sg.issueCheckErrors(new, urn, failures) {
						return nil, errors.New("One or more resource validation errors occurred; refusing to proceed")
					}
					new.Inputs = inputs
				}

				if logging.V(7) {
					logging.V(7).Infof("Planner decided to replace '%v' (oldprops=%v inputs=%v)",
						urn, oldInputs, new.Inputs)
				}

				// We have two approaches to performing replacements:
				//
				//     * CreateBeforeDelete: the default mode first creates a new instance of the resource, then
				//       updates all dependent resources to point to the new one, and finally after all of that,
				//       deletes the old resource.  This ensures minimal downtime.
				//
				//     * DeleteBeforeCreate: this mode can be used for resources that cannot be tolerate having
				//       side-by-side old and new instances alive at once.  This first deletes the resource and
				//       then creates the new one.  This may result in downtime, so is less preferred.  Note that
				//       until pulumi/pulumi#624 is resolved, we cannot safely perform this operation on resources
				//       that have dependent resources (we try to delete the resource while they refer to it).
				//
				// The provider is responsible for requesting which of these two modes to use.

				if diff.DeleteBeforeReplace {
					logging.V(7).Infof("Planner decided to delete-before-replacement for resource '%v'", urn)
					contract.Assert(sg.plan.depGraph != nil)

					// DeleteBeforeCreate implies that we must immediately delete the resource. For correctness,
					// we must also eagerly delete all resources that depend directly or indirectly on the resource
					// being replaced.
					//
					// To do this, we'll utilize the dependency information contained in the snapshot, which is
					// interpreted by the DependencyGraph type.
					var steps []Step
					dependents := sg.plan.depGraph.DependingOn(old)

					// Deletions must occur in reverse dependency order, and `deps` is returned in dependency
					// order, so we iterate in reverse.
					for i := len(dependents) - 1; i >= 0; i-- {
						dependentResource := dependents[i]

						// If we already deleted this resource due to some other DBR, don't do it again.
						if sg.deletes[urn] {
							continue
						}

						logging.V(7).Infof("Planner decided to delete '%v' due to dependence on condemned resource '%v'",
							dependentResource.URN, urn)
						steps = append(steps, NewDeleteReplacementStep(sg.plan, dependentResource, false))

						// Mark the condemned resource as deleted. We won't know until later in the plan whether
						// or not we're going to be replacing this resource.
						sg.deletes[dependentResource.URN] = true
					}

					return append(steps,
						NewDeleteReplacementStep(sg.plan, old, false),
						NewReplaceStep(sg.plan, old, new, diff.ReplaceKeys, false),
						NewCreateReplacementStep(sg.plan, event, old, new, diff.ReplaceKeys, false),
					), nil
				}

				return []Step{
					NewCreateReplacementStep(sg.plan, event, old, new, diff.ReplaceKeys, true),
					NewReplaceStep(sg.plan, old, new, diff.ReplaceKeys, true),
					// note that the delete step is generated "later" on, after all creates/updates finish.
				}, nil
			}

			// If we fell through, it's an update.
			sg.updates[urn] = true
			if logging.V(7) {
				logging.V(7).Infof("Planner decided to update '%v' (oldprops=%v inputs=%v", urn, oldInputs, new.Inputs)
			}
			return []Step{NewUpdateStep(sg.plan, event, old, new, diff.StableKeys)}, nil
		}

		// No need to update anything, the properties didn't change.
		sg.sames[urn] = true
		if logging.V(7) {
			logging.V(7).Infof("Planner decided not to update '%v' (same) (inputs=%v)", urn, new.Inputs)
		}
		return []Step{NewSameStep(sg.plan, event, old, new)}, nil
	}

	// Case 3: Not Case 1 or Case 2
	//  If a resource isn't being recreated and it's not being updated or replaced,
	//  it's just being created.
	sg.creates[urn] = true
	logging.V(7).Infof("Planner decided to create '%v' (inputs=%v)", urn, new.Inputs)
	return []Step{NewCreateStep(sg.plan, event, new)}, nil
}

func (sg *stepGenerator) GenerateDeletes() []Step {
	// To compute the deletion list, we must walk the list of old resources *backwards*.  This is because the list is
	// stored in dependency order, and earlier elements are possibly leaf nodes for later elements.  We must not delete
	// dependencies prior to their dependent nodes.
	var dels []Step
	if prev := sg.plan.prev; prev != nil {
		for i := len(prev.Resources) - 1; i >= 0; i-- {
			// If this resource is explicitly marked for deletion or wasn't seen at all, delete it.
			res := prev.Resources[i]
			if res.Delete {
				logging.V(7).Infof("Planner decided to delete '%v' due to replacement", res.URN)
				// The below assert is commented-out because it's believed to be wrong.
				//
				// The original justification for this assert is that the author (swgillespie) believed that
				// it was impossible for a single URN to be deleted multiple times in the same program.
				// This has empirically been proven to be false - it is possible using today engine to construct
				// a series of actions that puts arbitrarily many pending delete resources with the same URN in
				// the snapshot.
				//
				// It is not clear whether or not this is OK. I (swgillespie), the author of this comment, have
				// seen no evidence that it is *not* OK. However, concerns were raised about what this means for
				// structural resources, and so until that question is answered, I am leaving this comment and
				// assert in the code.
				//
				// Regardless, it is better to admit strange behavior in corner cases than it is to crash the CLI
				// whenever we see multiple deletes for the same URN.
				// contract.Assert(!sg.deletes[res.URN])
				if sg.deletes[res.URN] {
					logging.V(7).Infof(
						"Planner is deleting pending-delete urn '%v' that has already been deleted", res.URN)
				}
				sg.deletes[res.URN] = true
				dels = append(dels, NewDeleteReplacementStep(sg.plan, res, true))
			} else if !sg.sames[res.URN] && !sg.updates[res.URN] && !sg.replaces[res.URN] && !sg.deletes[res.URN] {
				// In addition to the above comment, I am fairly certain there is a bug here. If a resource
				// is not registered in a plan, but there exists a pending delete copy of that resource in the
				// snapshot, we will choose not to delete the live resource and instead be content with deleting
				// the pending delete resource.
				//
				// This is fairly benign, since in the worst case we'll delete the resource on the next plan, but
				// it points to a need for a more principled handling of pending deletions.
				logging.V(7).Infof("Planner decided to delete '%v'", res.URN)
				sg.deletes[res.URN] = true
				dels = append(dels, NewDeleteStep(sg.plan, res))
			}
		}
	}
	return dels

}

// diff returns a DiffResult for the given resource.
func (sg *stepGenerator) diff(urn resource.URN, id resource.ID, oldInputs, oldOutputs, newInputs, newOutputs,
	newProps resource.PropertyMap, prov plugin.Provider, refresh, allowUnknowns bool) (plugin.DiffResult, error) {

	// Workaround #1251: unexpected replaces.
	//
	// The legacy/desired behavior here is that if the provider-calculated inputs for a resource did not change,
	// then the resource itself should not change. Unfortunately, we (correctly?) pass the entire current state
	// of the resource to Diff, which includes calculated/output properties that may differ from those present
	// in the input properties. This can cause unexpected diffs.
	//
	// For now, simply apply the legacy diffing behavior before deferring to the provider.
	var hasChanges bool
	if refresh {
		hasChanges = !oldOutputs.DeepEquals(newOutputs)
	} else {
		hasChanges = !oldInputs.DeepEquals(newInputs)
	}
	if !hasChanges {
		return plugin.DiffResult{Changes: plugin.DiffNone}, nil
	}

	// If there is no provider for this resource, simply return a "diffs exist" result.
	if prov == nil {
		return plugin.DiffResult{Changes: plugin.DiffSome}, nil
	}

	// Grab the diff from the provider. At this point we know that there were changes to the Pulumi inputs, so if the
	// provider returns an "unknown" diff result, pretend it returned "diffs exist".
	diff, err := prov.Diff(urn, id, oldOutputs, newProps, allowUnknowns)
	if err != nil {
		return plugin.DiffResult{}, err
	}
	if diff.Changes == plugin.DiffUnknown {
		diff.Changes = plugin.DiffSome
	}
	return diff, nil
}

func (sg *stepGenerator) getResourcePropertyStates(urn resource.URN, goal *resource.Goal) (resource.PropertyMap,
	resource.PropertyMap, resource.PropertyMap, *resource.State) {
	props := goal.Properties
	var inputs resource.PropertyMap
	var outputs resource.PropertyMap
	if sg.plan.IsRefresh() {
		// In the case of a refresh, we will preserve the old inputs (since we won't have any new ones).  Note
		// that this can lead to a state in which inputs could not have possibly produced the outputs, but this
		// will need to be reconciled manually by the programmer updating the program accordingly.
		if old, ok := sg.plan.Olds()[urn]; ok {
			inputs = old.Inputs
		}
		outputs = props
	} else {
		// In the case of non-refreshes, outputs remain empty (they will be computed), but inputs are present.
		inputs = props
	}
	return props, inputs, outputs,
		resource.NewState(goal.Type, urn, goal.Custom, false, "",
			inputs, outputs, goal.Parent, goal.Protect, goal.Dependencies, []string{})

}

func (sg *stepGenerator) generateURN(e RegisterResourceEvent) resource.URN {
	// Use the resource goal state name to produce a globally unique URN.

	goal := e.Goal()
	parentType := tokens.Type("")
	if p := goal.Parent; p != "" && p.Type() != resource.RootStackType {
		// Skip empty parents and don't use the root stack type; otherwise, use the full qualified type.
		parentType = p.QualifiedType()
	}

	return resource.NewURN(sg.plan.Target().Name, sg.plan.source.Project(), parentType, goal.Type, goal.Name)
}

// issueCheckErrors prints any check errors to the diagnostics sink.
func (sg *stepGenerator) issueCheckErrors(new *resource.State, urn resource.URN,
	failures []plugin.CheckFailure) bool {
	if len(failures) == 0 {
		return false
	}
	inputs := new.Inputs
	for _, failure := range failures {
		if failure.Property != "" {
			sg.plan.Diag().Errorf(diag.GetResourcePropertyInvalidValueError(urn),
				new.Type, urn.Name(), failure.Property, inputs[failure.Property], failure.Reason)
		} else {
			sg.plan.Diag().Errorf(
				diag.GetResourceInvalidError(urn), new.Type, urn.Name(), failure.Reason)
		}
	}
	return true
}

// Provider fetches the provider for a given resource type, possibly lazily allocating the plugins for it.  If a
// provider could not be found, or an error occurred while creating it, a non-nil error is returned.
func (sg *stepGenerator) provider(t tokens.Type) (plugin.Provider, error) {
	pkg := t.Package()
	prov, err := sg.plan.Provider(pkg)
	if err != nil {
		return nil, err
	} else if prov == nil {
		return nil, errors.Errorf("could not load resource provider for package '%v' from $PATH", pkg)
	}
	return prov, nil
}

func (sg *stepGenerator) Creates() map[resource.URN]bool  { return sg.creates }
func (sg *stepGenerator) Sames() map[resource.URN]bool    { return sg.sames }
func (sg *stepGenerator) Updates() map[resource.URN]bool  { return sg.updates }
func (sg *stepGenerator) Replaces() map[resource.URN]bool { return sg.replaces }
func (sg *stepGenerator) Deletes() map[resource.URN]bool  { return sg.deletes }

// newStepGenerator creates a new step generator that operates on the given plan.
func newStepGenerator(plan *Plan, opts Options) *stepGenerator {
	return &stepGenerator{
		plan:     plan,
		opts:     opts,
		urns:     make(map[resource.URN]bool),
		creates:  make(map[resource.URN]bool),
		sames:    make(map[resource.URN]bool),
		replaces: make(map[resource.URN]bool),
		updates:  make(map[resource.URN]bool),
		deletes:  make(map[resource.URN]bool),
	}
}
