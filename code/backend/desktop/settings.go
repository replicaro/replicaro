package desktop

import (
	"database/sql"
	"fmt"

	"github.com/local/replicaro/database"
)

func ApplySettings(
	db *sql.DB,
) error {

	settings, err :=
		database.GetSettings(db)

	if err != nil {
		return err
	}

	if settings.StartWithWindows || settings.StartAtLogin {
		if err := RegisterStartup(); err != nil {
			return fmt.Errorf("register startup-at-login: %w", err)
		}

	} else {
		if err := UnregisterStartup(); err != nil {
			return fmt.Errorf("remove startup-at-login: %w", err)
		}
	}

	_ = database.LogActivity(
		db,
		"Desktop settings applied",
	)

	return nil
}
