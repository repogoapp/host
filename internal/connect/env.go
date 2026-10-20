package connect

// An env key: one secret step, saved as a line of a dotenv file the host finds.

func (s *Service) envStatus(project string, key envKey, c *Connection) error {
	file, err := s.resolve(project, key.Scope)
	if err != nil {
		return err
	}
	c.Destination, c.Status = file.Destination, Missing
	if hasKey(file.Path, key.Name) {
		c.Status = Configured
	}
	return nil
}

func (s *Service) startEnv(f *flow, note, help string) error {
	file, err := s.resolve(f.project, f.target.Env.Scope)
	if err != nil {
		return err
	}
	name := f.target.Env.Name
	f.Destination = file.Destination
	f.Step = &Step{ID: "secret", Kind: StepSecret, Label: name, Name: name, Note: note, HelpURL: help,
		Replaces: hasKey(file.Path, name)}
	return nil
}

func (s *Service) submitEnv(f flow, value string) error {
	line, err := envLine(f.target.Env.Name, value)
	if err != nil {
		return err
	}
	file, err := s.resolve(f.project, f.target.Env.Scope)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeKey(file.Path, f.target.Env.Name, line)
}
