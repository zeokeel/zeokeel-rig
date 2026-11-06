package assembly

import (
	"github.com/zeokeel/zeokeel-rig/kernelx"
)

// GroupModuleID is the stable module identity of the no-op group container.
const GroupModuleID = "rig/host/group"

// GroupConfig is the (empty) typed config of a group entry.
type GroupConfig struct{}

// GroupModule returns the no-op module that carries an Entry.Children subtree.
//
// A group owns no services and runs no logic. It exists so an assembly can nest
// a subtree under one entry that can be configured, disabled and disposed as a
// unit: disposing the group disposes the whole subtree through the kernel's
// child ownership, which runs every child's effects (registrations, cleanups)
// in reverse order.
func GroupModule() *kernelx.Descriptor[GroupConfig] {
	return kernelx.Define(GroupModuleID, func(kernelx.Context, GroupConfig) error {
		return nil
	}).WithName("Rig module group")
}
