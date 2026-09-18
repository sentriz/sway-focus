## sway-focus

focus history for sway, with optional browser tab support through [bruvtab](https://github.com/pschmitt/bruvtab)

You can switch between both windows and browser tabs. Going back will focus the previous tab as well as its window.

If happen to use a launcher to switch windows, you can also use this to switch between tabs as well as windows.

It makes tabs feel a lot more like native programs, so you don't need to remember which was a window and which was a tab.

## usage

Run the daemon, and bind back to a key:

    exec sway-focus
    bindsym $super+grave exec sway-focus back

List every window and tab, one per line:

    $ sway-focus list | table " | "
    12:librewolf:     | 2 | 1 | librewolf | YouTube — LibreWolf        |
    12:librewolf:1.5  | 2 | 1 | librewolf | YouTube                    | https://youtube.com/
    12:librewolf:1.3  | 2 |   | librewolf | Example Domain             | https://example.com/
    15::              | 3 |   | foot      | ~ fish                     |

The columns are location, workspace, visible, app id, title, and URL. A location is `<con>:<browser>:<tab>`, and visible
means the window is on screen, or for a tab that it's the one its window is showing. Each browser window is followed by
its tabs. Pipe it to a menu and focus whatever was picked:

    $ sway-focus focus 12:librewolf:1.3

## bruvtab

With no config only windows are tracked. For tabs, install the bruvtab extension and its mediator, giving each browser
its own port with `MIN_HTTP_PORT` and `MAX_HTTP_PORT`. Then name them in `$XDG_CONFIG_HOME/sway-focus`:

    browser librewolf     bruvtab http://localhost:4625
    browser google-chrome bruvtab http://localhost:4626

A browser in flatpak can't run the mediator. Its manifest, under `~/.var/app/<app id>/`, should point at a script in the
sandbox that runs the mediator on the host, and the browser needs the `org.freedesktop.Flatpak` talk override:

    #!/bin/sh
    cd /
    exec flatpak-spawn --host --env=MIN_HTTP_PORT=4625 --env=MAX_HTTP_PORT=4626 \
        "$HOME/.local/bin/bruvtab_mediator" "$@"

The manifest path is where the browser sees the script, so for firefox that's inside its own home. None of this is
needed once browsers support the WebExtensions portal.
