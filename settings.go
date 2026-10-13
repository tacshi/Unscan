package main

import (
	"time"

	"github.com/egoist/mygo/ui"
)

// updatePrefs are the user's choices about updates, which package updater
// keeps, and whether this copy of the app can update itself at all.
type updatePrefs struct {
	enabled   bool      // a release build, in a folder it can write to
	checks    bool      // check for updates in the background, once a day
	downloads bool      // install those found without asking, for the next launch
	lastCheck time.Time // when the app last checked, or zero
}

// openSettings shows the fonts retyped PDFs are set in, and how the app
// updates.
func (a *app) openSettings() { a.settingsOpen = true }

// updatesChanged keeps the user's choices about updates.
func (a *app) updatesChanged() { a.fx.setUpdates(a.updates) }

// settingsDialog shows the fonts retyped PDFs are set in, each face's
// built-in one or a font file the user chose, which they may change, and
// whether the app checks for updates and installs them by itself.
func (a *app) settingsDialog(c *ui.Context) {
	t := c.Theme()
	ui.Modal(c, &a.settingsOpen, func() {
		ui.Column(c).Width(560).MaxWidthPercent(100).Gap(16).Children(func() {
			ui.Text(c, "Settings").FontSize(17).Bold()
			ui.Column(c).Gap(4).Children(func() {
				ui.Text(c, "Fonts").Bold()
				ui.Text(c, "Retyped PDFs set each line in a font of the kind and weight it was printed in. "+
					"Choose TrueType fonts (.ttf or .ttc) to use instead; the built-in fonts set the characters they lack. "+
					"Fonts apply to the PDFs read from now on.").
					TextColor(t.TextMuted)
			})
			// One form, so that the labels of both line up.
			ui.Form(c, func() {
				for i := range fontFaces {
					a.fontField(c, i)
				}
				ui.Divider(c)
				a.updatesField(c)
			})
			ui.Row(c).Justify(ui.End).Children(func() {
				ui.PrimaryButton(c, "Done").OnClick(func() { a.settingsOpen = false })
			})
		})
	})
}

// updatesField shows whether the app checks for updates and installs them
// by itself, which the user may change, and when it last checked.
func (a *app) updatesField(c *ui.Context) {
	t := c.Theme()
	u := &a.updates
	field := ui.Field(c.Key("updates"), "Updates", func() {
		ui.Column(c).Gap(8).Children(func() {
			ui.Checkbox(c.Key("checks"), &u.checks, "Check for updates automatically").
				Disabled(!u.enabled).OnChange(a.updatesChanged)
			ui.Checkbox(c.Key("downloads"), &u.downloads, "Download and install updates automatically").
				Disabled(!u.enabled || !u.checks).OnChange(a.updatesChanged)
			ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Button(c.Key("check"), "Check Now").OnClick(a.fx.checkUpdates)
				if u.enabled {
					last := "Not checked yet"
					if !u.lastCheck.IsZero() {
						last = "Last checked: " + when(u.lastCheck, time.Now())
					}
					ui.Text(c, last).FontSize(12).TextColor(t.TextMuted).Grow(1).MinWidth(0).SingleLine()
				}
			})
		})
	})
	if !u.enabled {
		field.Description("This copy of Unscan can’t update itself: it’s a development build, or it’s in a folder it can’t change.")
	}
}
