// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2024 Canonical Ltd
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

package confdb

var GetValuesThroughPaths = getValuesThroughPaths

func MockMaxValueDepth(newDepth int) (restore func()) {
	oldDepth := maxValueDepth
	maxValueDepth = newDepth
	return func() {
		maxValueDepth = oldDepth
	}
}

// isValidAuthenticationMethod exposed for tests
var IsValidAuthenticationMethod = isValidAuthenticationMethod

// convertToAuthenticationMethods exposed for tests
var ConvertToAuthenticationMethods = convertToAuthenticationMethods

// groupWithView exposed for tests
func (o *Operator) GroupWithView(view *ViewRef) (*ControlGroup, int) {
	return o.groupWithView(view)
}

// groupWithAuthentication exposed for tests
func (o *Operator) GroupWithAuthentication(auth []AuthenticationMethod) *ControlGroup {
	return o.groupWithAuthentication(auth)
}

// compare exposed for tests
func (v *ViewRef) Compare(b *ViewRef) int {
	return v.compare(b)
}
