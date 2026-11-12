package model

// A remote disappearance is confirmed by a successful complete parent listing,
// never by a download failure, timeout, or permission error alone.
const ConflictRemoteDeleted = "remote_deleted"
const ResolveRestoreLocal = "restore_local"
const ResolveAcceptDelete = "accept_delete"

func DeletedRevision(token string) string { return "deleted:" + token }
