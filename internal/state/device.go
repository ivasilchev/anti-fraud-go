package state

import "slices"

// DevicePlayer records the last time a player was seen on a device.
type DevicePlayer struct {
	Player    uint32
	LastSeenT int64
}

// Device is the rule state for one device. The zero value has no history.
type Device struct {
	Declines []Decline
	Players  []DevicePlayer // one entry per player
}

// AddDecline records a DEPOSIT_DECLINED made from this device.
func (d *Device) AddDecline(t int64, pm uint32, now int64) {
	d.Declines = append(pruneDeclines(d.Declines, now-DeclinesRetentionMs), Decline{T: t, PM: pm})
}

// SeePlayer records that player used this device at t (max-of), pruning
// players not seen within DevicePlayersRetentionMs of now.
func (d *Device) SeePlayer(player uint32, t, now int64) {
	cutoff := now - DevicePlayersRetentionMs
	d.Players = slices.DeleteFunc(d.Players, func(e DevicePlayer) bool { return e.LastSeenT < cutoff })
	for i := range d.Players {
		e := &d.Players[i]
		if e.Player == player {
			e.LastSeenT = max(e.LastSeenT, t)
			return
		}
	}
	d.Players = append(d.Players, DevicePlayer{Player: player, LastSeenT: t})
}
