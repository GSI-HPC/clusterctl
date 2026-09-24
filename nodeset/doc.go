// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

// Package nodeset parses, folds and expands ClusterShell style node sets.
//
// A node set expression names a set of hosts. Numeric parts of a host name
// may be written as a bracketed range, and several expressions may be
// combined with set operators:
//
//	exe[0001-0010]            a padded range
//	exe[1-10/2]               a range with a step
//	rack[1-2]node[01-04]      two independent numeric dimensions
//	@compute                  a group, resolved by a Resolver
//	exe[1-10],sub[1-2]        union
//	exe[1-10]!exe5            difference
//	exe[1-10]&@idle           intersection
//	exe[1-10]^@drained        symmetric difference
//
// Operators have no precedence; an expression is evaluated strictly from left
// to right. Whitespace acts as a union operator, so the arguments of a command
// line may be joined with a space and parsed in one call. The operators !, &
// and ^ need an operand on each side: "exe[1-10]&" is an error, not exe[1-10].
//
// # Node names
//
// Every maximal run of digits in a name is a numeric dimension, whether or not
// it was written in brackets. "exe0001" and "exe[0001]" parse identically, and
// "10.0.1.7" has four dimensions. A dimension holding a single value is
// rendered without brackets, so folding is stable: parsing the output of
// String and folding it again yields the same string.
//
// Zero padding is not part of a node's identity: "exe1" and "exe01" are the
// same host, so Parse("exe1,exe01") holds one host and Contains("exe01") is
// true for a set holding exe1. Each host keeps the spelling it was first
// given, and a set never shows a host under a name it was not given:
// "exe[01-02],exe3" prints as exe[01-02,3], and Canonical("exe1") on a set
// holding exe0001 answers "exe0001". In a range the padding of the first bound
// applies to the whole range, and a last bound padded to another width, such
// as exe[1-010], is an error.
package nodeset
