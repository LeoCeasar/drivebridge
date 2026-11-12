package model

import "strconv"

// A remote disappearance is confirmed by a successful complete parent listing,
// never by a download failure, timeout, or permission error alone.
const ConflictRemoteDeleted = "remote_deleted"
const ConflictLocalDeletePending = "local_delete_pending"
const ResolveRestoreLocal = "restore_local"
const ResolveAcceptDelete = "accept_delete"

func DeletedRevision(token string) string { return "deleted:" + token }

func LocalDeletionRevision(id EntryID, generation uint64) string {
	return "local-delete:" + string(id) + ":" + strconv.FormatUint(generation, 10)
}
