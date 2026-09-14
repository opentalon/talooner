package facts

type Set map[string]any

func New() Set {
	return make(Set)
}

func (s Set) Bool(name string, v bool) {
	s[name] = v
}

func (s Set) Int(name string, v int) {
	s[name] = v
}

func (s Set) String(name string, v string) {
	s[name] = v
}

func (s Set) Strings(name string, v []string) {
	if v == nil {
		v = []string{}
	}
	s[name] = v
}
