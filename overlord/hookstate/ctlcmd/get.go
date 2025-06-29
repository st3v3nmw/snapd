// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2016 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package ctlcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/snapcore/snapd/confdb"
	"github.com/snapcore/snapd/features"
	"github.com/snapcore/snapd/i18n"
	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/overlord/confdbstate"
	"github.com/snapcore/snapd/overlord/configstate"
	"github.com/snapcore/snapd/overlord/configstate/config"
	"github.com/snapcore/snapd/overlord/hookstate"
	"github.com/snapcore/snapd/overlord/ifacestate/ifacerepo"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/snap"
)

var (
	confdbstateGetView           = confdbstate.GetView
	confdbstateTransactionForGet = confdbstate.GetTransactionForSnapctlGet
)

type getCommand struct {
	baseCommand

	// these two options are mutually exclusive
	ForceSlotSide bool `long:"slot" description:"return attribute values from the slot side of the connection"`
	ForcePlugSide bool `long:"plug" description:"return attribute values from the plug side of the connection"`
	View          bool `long:"view" description:"return confdb values from the view declared in the plug"`
	Previous      bool `long:"previous" description:"return confdb values disregarding changes from the current transaction"`

	Positional struct {
		PlugOrSlotSpec string   `positional-args:"true" positional-arg-name:":<plug|slot>"`
		Keys           []string `positional-arg-name:"<keys>" description:"option keys"`
	} `positional-args:"yes"`

	Document bool `short:"d" description:"always return document, even with single key"`
	Typed    bool `short:"t" description:"strict typing with nulls and quoted strings"`
}

var shortGetHelp = i18n.G("Print either configuration options or interface connection settings")
var longGetHelp = i18n.G(`
The get command prints configuration options for the current snap.

    $ snapctl get username
    frank

If multiple option names are provided, a document is returned:

    $ snapctl get username password
    {
        "username": "frank",
        "password": "..."
    }

Nested values may be retrieved via a dotted path:

    $ snapctl get author.name
    frank

Values of interface connection settings may be printed with:

    $ snapctl get :myplug usb-vendor
    $ snapctl get :myslot path

This will return the named setting from the local interface endpoint,
regardless whether it's a plug or a slot. Returning the setting from the
connected snap's endpoint is also possible by requesting the setting explicitly
with optional --plug and --slot command options:

    $ snapctl get :myplug --slot usb-vendor

This requests the "usb-vendor" setting from the slot that is connected to
"myplug".
`)

func init() {
	addCommand("get", shortGetHelp, longGetHelp, func() command {
		return &getCommand{}
	})
}

func (c *getCommand) printValues(getByKey func(string) (any, bool, error)) error {
	patch := make(map[string]any)
	for _, key := range c.Positional.Keys {
		value, output, err := getByKey(key)
		if err == nil {
			if output {
				patch[key] = value
			} // else skip this value
		} else {
			return err
		}
	}

	return c.printPatch(patch)
}

func (c *getCommand) printPatch(patch any) error {
	var confToPrint any = patch
	if !c.Document && len(c.Positional.Keys) == 1 {
		if confMap, ok := patch.(map[string]any); ok {
			confToPrint = confMap[c.Positional.Keys[0]]
		}
	}

	if c.Typed && confToPrint == nil {
		c.printf("null\n")
		return nil
	}

	if s, ok := confToPrint.(string); ok && !c.Typed {
		c.printf("%s\n", s)
		return nil
	}

	var bytes []byte
	if confToPrint != nil {
		var err error
		bytes, err = json.MarshalIndent(confToPrint, "", "\t")
		if err != nil {
			return err
		}
	}

	c.printf("%s\n", string(bytes))

	return nil
}

func (c *getCommand) Execute(args []string) error {
	if len(c.Positional.Keys) == 0 && c.Positional.PlugOrSlotSpec == "" {
		return errors.New(i18n.G("get which option?"))
	}

	context, err := c.ensureContext()
	if err != nil {
		return err
	}

	if c.Typed && c.Document {
		return fmt.Errorf("cannot use -d and -t together")
	}
	if c.Previous {
		if !c.View {
			return fmt.Errorf("cannot use --previous without --view")
		}

		hookPref := func(p string) bool { return strings.HasPrefix(context.HookName(), p) }
		if context == nil || context.IsEphemeral() || !(hookPref("save-view-") ||
			hookPref("change-view-") || hookPref("observe-view-")) {
			return fmt.Errorf(`cannot use --previous outside of save-view, change-view or observe-view hooks`)
		}
	}

	if strings.Contains(c.Positional.PlugOrSlotSpec, ":") {
		parts := strings.SplitN(c.Positional.PlugOrSlotSpec, ":", 2)
		snap, name := parts[0], parts[1]
		if name == "" {
			return fmt.Errorf("plug or slot name not provided")
		}
		if snap != "" {
			return fmt.Errorf(`"snapctl get %s" not supported, use "snapctl get :%s" instead`, c.Positional.PlugOrSlotSpec, parts[1])
		}

		if c.View {
			if err := validateConfdbsFeatureFlag(context.State()); err != nil {
				return err
			}

			requests := c.Positional.Keys
			return c.getConfdbValues(context, name, requests, c.Previous)
		}

		if len(c.Positional.Keys) == 0 {
			return errors.New(i18n.G("get which attribute?"))
		}

		return c.getInterfaceSetting(context, name)
	}

	// PlugOrSlotSpec is actually a configuration key.
	c.Positional.Keys = append([]string{c.Positional.PlugOrSlotSpec}, c.Positional.Keys[0:]...)
	c.Positional.PlugOrSlotSpec = ""

	return c.getConfigSetting(context)
}

func (c *getCommand) getConfigSetting(context *hookstate.Context) error {
	if c.ForcePlugSide || c.ForceSlotSide {
		return fmt.Errorf("cannot use --plug or --slot without <snap>:<plug|slot> argument")
	}

	context.Lock()
	transaction := configstate.ContextTransaction(context)
	context.Unlock()

	return c.printValues(func(key string) (any, bool, error) {
		var value any
		err := transaction.Get(c.context().InstanceName(), key, &value)
		if err == nil {
			return value, true, nil
		}
		if config.IsNoOption(err) {
			if !c.Typed {
				value = ""
			}
			return value, false, nil
		}
		return value, false, err
	})
}

type ifaceHookType int

const (
	preparePlugHook ifaceHookType = iota
	prepareSlotHook
	unpreparePlugHook
	unprepareSlotHook
	connectPlugHook
	connectSlotHook
	disconnectPlugHook
	disconnectSlotHook
	unknownHook
)

func interfaceHookType(hookName string) (ifaceHookType, error) {
	switch {
	case strings.HasPrefix(hookName, "prepare-plug-"):
		return preparePlugHook, nil
	case strings.HasPrefix(hookName, "connect-plug-"):
		return connectPlugHook, nil
	case strings.HasPrefix(hookName, "prepare-slot-"):
		return prepareSlotHook, nil
	case strings.HasPrefix(hookName, "connect-slot-"):
		return connectSlotHook, nil
	case strings.HasPrefix(hookName, "disconnect-plug-"):
		return disconnectPlugHook, nil
	case strings.HasPrefix(hookName, "disconnect-slot-"):
		return disconnectSlotHook, nil
	case strings.HasPrefix(hookName, "unprepare-slot-"):
		return unprepareSlotHook, nil
	case strings.HasPrefix(hookName, "unprepare-plug-"):
		return unpreparePlugHook, nil
	default:
		return unknownHook, fmt.Errorf("unknown hook type")
	}
}

func validatePlugOrSlot(attrsTask *state.Task, plugSide bool, plugOrSlot string) error {
	// check if the requested plug or slot is correct for given hook.
	attrsTask.State().Lock()
	defer attrsTask.State().Unlock()

	var name string
	var err error
	if plugSide {
		var plugRef interfaces.PlugRef
		if err = attrsTask.Get("plug", &plugRef); err == nil {
			name = plugRef.Name
		}
	} else {
		var slotRef interfaces.SlotRef
		if err = attrsTask.Get("slot", &slotRef); err == nil {
			name = slotRef.Name
		}
	}
	if err != nil {
		return errors.New(i18n.G("internal error: cannot find plug or slot data in the appropriate task"))
	}
	if name != plugOrSlot {
		return fmt.Errorf(i18n.G("unknown plug or slot %q"), plugOrSlot)
	}
	return nil
}

func attributesTask(context *hookstate.Context) (*state.Task, error) {
	var attrsTaskID string
	context.Lock()
	defer context.Unlock()

	if err := context.Get("attrs-task", &attrsTaskID); err != nil {
		return nil, err
	}

	st := context.State()

	attrsTask := st.Task(attrsTaskID)
	if attrsTask == nil {
		return nil, errors.New(i18n.G("internal error: cannot find attrs task"))
	}

	return attrsTask, nil
}

func (c *getCommand) getInterfaceSetting(context *hookstate.Context, plugOrSlot string) error {
	// Make sure get :<plug|slot> is only supported during the execution of interface hooks
	hookType, err := interfaceHookType(context.HookName())
	if err != nil {
		return errors.New(i18n.G("interface attributes can only be read during the execution of interface hooks"))
	}

	attrsTask, err := attributesTask(context)
	if err != nil {
		return err
	}

	if c.ForcePlugSide && c.ForceSlotSide {
		return fmt.Errorf("cannot use --plug and --slot together")
	}

	isPlugSide := (hookType == preparePlugHook || hookType == unpreparePlugHook || hookType == connectPlugHook || hookType == disconnectPlugHook)
	if err = validatePlugOrSlot(attrsTask, isPlugSide, plugOrSlot); err != nil {
		return err
	}

	var which string
	if c.ForcePlugSide || (isPlugSide && !c.ForceSlotSide) {
		which = "plug"
	} else {
		which = "slot"
	}

	st := context.State()
	st.Lock()
	defer st.Unlock()

	var staticAttrs, dynamicAttrs map[string]any
	if err = attrsTask.Get(which+"-static", &staticAttrs); err != nil {
		return fmt.Errorf(i18n.G("internal error: cannot get %s from appropriate task"), which)
	}
	if err = attrsTask.Get(which+"-dynamic", &dynamicAttrs); err != nil {
		return fmt.Errorf(i18n.G("internal error: cannot get %s from appropriate task"), which)
	}

	return c.printValues(func(key string) (any, bool, error) {
		subkeys, err := config.ParseKey(key)
		if err != nil {
			return nil, false, err
		}

		var value any
		err = getAttribute(context.InstanceName(), subkeys, 0, staticAttrs, &value)
		if err == nil {
			return value, true, nil
		}
		if isNoAttribute(err) {
			err = getAttribute(context.InstanceName(), subkeys, 0, dynamicAttrs, &value)
			if err == nil {
				return value, true, nil
			}
		}
		return nil, false, err
	})
}

func (c *getCommand) getConfdbValues(ctx *hookstate.Context, plugName string, requests []string, previous bool) error {
	if c.ForcePlugSide || c.ForceSlotSide {
		return errors.New(i18n.G("cannot use --plug or --slot with --view"))
	}
	ctx.Lock()
	defer ctx.Unlock()

	account, dbSchemaName, viewName, err := getConfdbViewID(ctx, plugName)
	if err != nil {
		return err
	}

	view, err := confdbstateGetView(ctx.State(), account, dbSchemaName, viewName)
	if err != nil {
		return err
	}

	tx, err := confdbstateTransactionForGet(ctx, view, requests)
	if err != nil {
		return err
	}

	var bag confdb.Databag = tx
	if previous {
		bag = tx.Previous()
	}

	res, err := confdbstate.GetViaView(bag, view, requests)
	if err != nil {
		return err
	}

	return c.printPatch(res)
}

func getConfdbViewID(ctx *hookstate.Context, plugName string) (account, dbSchemaName, viewName string, err error) {
	repo := ifacerepo.Get(ctx.State())

	plug := repo.Plug(ctx.InstanceName(), plugName)
	if plug == nil {
		return "", "", "", fmt.Errorf(i18n.G("cannot find plug :%s for snap %q"), plugName, ctx.InstanceName())
	}

	if plug.Interface != "confdb" {
		return "", "", "", fmt.Errorf(i18n.G("cannot use --view with non-confdb plug :%s"), plugName)
	}

	account, dbSchemaName, viewName, err = snap.ConfdbPlugAttrs(plug)
	if err != nil {
		return "", "", "", fmt.Errorf(i18n.G("invalid plug :%s: %w"), plugName, err)
	}

	return account, dbSchemaName, viewName, nil
}

// validateConfdbsFeatureFlag checks whether the confdb experimental flag
// is enabled. The state should not be locked by the caller.
func validateConfdbsFeatureFlag(st *state.State) error {
	st.Lock()
	defer st.Unlock()

	tr := config.NewTransaction(st)
	enabled, err := features.Flag(tr, features.Confdb)
	if err != nil && !config.IsNoOption(err) {
		return fmt.Errorf(i18n.G("internal error: cannot check confdb feature flag: %v"), err)
	}

	if !enabled {
		_, confName := features.Confdb.ConfigOption()
		return fmt.Errorf(i18n.G(`"confdb" feature flag is disabled: set '%s' to true`), confName)
	}

	return nil
}
