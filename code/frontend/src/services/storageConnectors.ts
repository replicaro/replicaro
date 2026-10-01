// Storage types whose behavior is fixed by connector ID. Each list below
// mirrors a fixed list in the backend; storageConnectors.test.ts reads those
// backend lists from their Go sources and fails when the two drift apart.

export const RCLONE_REMOTE_CONNECTOR = "rclone_remote";

// Signs in through rclone in the browser before a vault is created or
// connected (engines.rcloneNativeLoginProvider). Any Rclone Remote is not one
// of them: the user sets up the remote in their own rclone.conf.
export const rcloneSignInConnectors: readonly string[] = ["dropbox", "google_drive", "onedrive"];

// A vault keeps exactly one profile, so Connect reuses or takes over that
// profile rather than offering Join or a choice of profiles
// (automaticSoleProfileConnector in api/repository_recovery.go and
// profilebinding/repair.go). WebDAV is not listed: it is an ordinary server
// connector like SFTP and keeps Join and multiple profiles.
export const singleProfileConnectors: readonly string[] = [...rcloneSignInConnectors, RCLONE_REMOTE_CONNECTOR];

// Restic through rclone, where the vault is the folder Replicaro/<vault name>
// and the vault name is that folder's name (engines.IsResticRcloneConnector).
// These are Restic only.
export const vaultFolderConnectors: readonly string[] = [...rcloneSignInConnectors, RCLONE_REMOTE_CONNECTOR];

// Capped at the Normal speed (models.NormalizeConcurrencyModeForConnector).
// The value is the name the speed help shows.
export const limitedSpeedProviders: Readonly<Record<string, string>> = {
	dropbox: "Dropbox",
	google_drive: "Google Drive",
	onedrive: "OneDrive",
	[RCLONE_REMOTE_CONNECTOR]: "Any Rclone Remote",
};

export const usesRcloneSignIn = (connector: string) => rcloneSignInConnectors.includes(connector);
export const usesSingleProfile = (connector: string) => singleProfileConnectors.includes(connector);
export const usesVaultFolderName = (connector: string) => vaultFolderConnectors.includes(connector);
