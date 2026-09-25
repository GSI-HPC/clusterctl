// SPDX-FileCopyrightText: 2026 GSI Helmholtz Centre for Heavy Ion Research GmbH <http://www.gsi.de>
// SPDX-License-Identifier: LGPL-3.0-or-later

package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/GSI-HPC/clusterctl/internal/safety"
)

// effects says what every command does. It is one table rather than a mark on
// each command so that the whole surface can be reviewed at once, and a test
// fails when a command is added without an entry.
//
// A command whose effect depends on its arguments is a change: bmc power
// with the status action only reads, but the table cannot see the argument,
// and bmc status asks the same question.
var effects = map[string]safety.Effect{
	"bmc boot set":              safety.EffectChange,
	"bmc boot show":             safety.EffectRead,
	"bmc boot unset":            safety.EffectChange,
	"bmc forget":                safety.EffectChange,
	"bmc ping":                  safety.EffectRead,
	"bmc power":                 safety.EffectChange,
	"bmc redfish get":           safety.EffectRead,
	"bmc redfish info":          safety.EffectRead,
	"bmc redfish post":          safety.EffectChange,
	"bmc status":                safety.EffectRead,
	"bmc web":                   safety.EffectInteractive,
	"boot grub set":             safety.EffectChange,
	"boot grub show":            safety.EffectRead,
	"boot grub unset":           safety.EffectChange,
	"boot list":                 safety.EffectRead,
	"boot log":                  safety.EffectRead,
	"boot set":                  safety.EffectChange,
	"boot shell":                safety.EffectInteractive,
	"boot status":               safety.EffectRead,
	"boot sync":                 safety.EffectChange,
	"boot unset":                safety.EffectChange,
	"cinc config":               safety.EffectChange,
	"cinc run":                  safety.EffectChange,
	"cinc shell":                safety.EffectInteractive,
	"cinc show":                 safety.EffectRead,
	"completion bash":           safety.EffectRead,
	"completion fish":           safety.EffectRead,
	"completion powershell":     safety.EffectRead,
	"completion zsh":            safety.EffectRead,
	"config contexts":           safety.EffectRead,
	"config explain":            safety.EffectRead,
	"config init":               safety.EffectChange,
	"config schema":             safety.EffectRead,
	"config use-context":        safety.EffectRead,
	"config validate":           safety.EffectRead,
	"config view":               safety.EffectRead,
	"copy":                      safety.EffectChange,
	"dhcp capture":              safety.EffectInteractive,
	"dhcp config":               safety.EffectRead,
	"dhcp hosts":                safety.EffectRead,
	"dhcp log":                  safety.EffectRead,
	"dhcp shell":                safety.EffectInteractive,
	"dns aliases":               safety.EffectRead,
	"dns lookup":                safety.EffectRead,
	"doctor":                    safety.EffectRead,
	"exec":                      safety.EffectChange,
	"fabric counters":           safety.EffectRead,
	"fabric guid":               safety.EffectRead,
	"fabric state":              safety.EffectRead,
	"hca cable":                 safety.EffectRead,
	"hca config get":            safety.EffectRead,
	"hca config set":            safety.EffectChange,
	"hca firmware":              safety.EffectRead,
	"hca link":                  safety.EffectRead,
	"help":                      safety.EffectRead,
	"hostkey list":              safety.EffectRead,
	"hostkey refresh":           safety.EffectChange,
	"hostkey remove":            safety.EffectChange,
	"hostkey scan":              safety.EffectRead,
	"hostkey verify":            safety.EffectRead,
	"login":                     safety.EffectInteractive,
	"mcp serve":                 safety.EffectInteractive,
	"node attrs":                safety.EffectRead,
	"node describe":             safety.EffectRead,
	"node fqdn":                 safety.EffectRead,
	"node groups":               safety.EffectRead,
	"node hw":                   safety.EffectRead,
	"node list":                 safety.EffectRead,
	"node rack":                 safety.EffectRead,
	"node select":               safety.EffectRead,
	"pdu list":                  safety.EffectRead,
	"pdu shell":                 safety.EffectInteractive,
	"provision reinstall":       safety.EffectChange,
	"provision status":          safety.EffectRead,
	"secrets check":             safety.EffectRead,
	"secrets list":              safety.EffectRead,
	"secrets push":              safety.EffectChange,
	"slurm account add":         safety.EffectChange,
	"slurm account coordinator": safety.EffectChange,
	"slurm account limits":      safety.EffectRead,
	"slurm account list":        safety.EffectRead,
	"slurm account shares":      safety.EffectChange,
	"slurm job history":         safety.EffectRead,
	"slurm job list":            safety.EffectRead,
	"slurm job summary":         safety.EffectRead,
	"slurm node drain":          safety.EffectChange,
	"slurm node list":           safety.EffectRead,
	"slurm node nodeset":        safety.EffectRead,
	"slurm node resume":         safety.EffectChange,
	"slurm partition":           safety.EffectRead,
	"slurm user add":            safety.EffectChange,
	"slurm user default":        safety.EffectChange,
	"slurm user list":           safety.EffectRead,
	"tunnel list":               safety.EffectRead,
	"tunnel start":              safety.EffectInteractive,
	"tunnel status":             safety.EffectRead,
	"tunnel stop":               safety.EffectChange,
	"version":                   safety.EffectRead,
}

// commandPath names a command the way the effects table does: the words
// after the program name.
func commandPath(cmd *cobra.Command) string {
	path := cmd.CommandPath()
	if i := strings.IndexByte(path, ' '); i >= 0 {
		return path[i+1:]
	}
	return ""
}

// annotateEffects records the effect of every leaf command on the command
// itself. A leaf missing from the table is left unmarked, which everything
// reading the mark treats as a change.
func annotateEffects(cmd *cobra.Command) {
	for _, sub := range cmd.Commands() {
		annotateEffects(sub)
	}
	if cmd.HasSubCommands() {
		return
	}
	effect, ok := effects[commandPath(cmd)]
	if !ok {
		return
	}
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[safety.EffectAnnotation] = string(effect)
}
