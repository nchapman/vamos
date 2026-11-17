package firewall

// SSHAllowances reads the owned management-port reservation without running
// firewall commands. Deployment uses it to avoid publishing over SSH while
// managed mode is enabled or awaiting confirmation. No receipt means the user
// owns firewall policy and there are no managed reservations.
func SSHAllowances(root, hostID string) ([]int, error) {
	m := &Manager{Root: root}
	s, err := m.load(hostID)
	if err != nil || s == nil {
		return nil, err
	}
	return append([]int(nil), s.Plan.SSHPorts...), nil
}
