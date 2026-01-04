package sankey

// Section represents a section in the sankey diagram
type Section int

const (
	SectionPowerhouseIn Section = iota
	SectionPowerhouse
	SectionPowerhouseOut
	SectionHouseMainsIn
	SectionHouseMains
	SectionHouseMainsOut
)

// TemplateType represents the type of template calculation
type TemplateType int

const (
	TemplateFormula TemplateType = iota
	TemplateSum
)

// SensorTemplate defines a calculated sensor template
type SensorTemplate struct {
	Name     string
	Type     TemplateType
	Formula  string   // Used when Type == TemplateFormula
	Entities []string // Used when Type == TemplateSum
}

// Sensor represents a sensor entity in a group
type Sensor struct {
	Name  string
	Label string // Optional display label
}

// ShouldBe represents the comparison type for reconciliation
type ShouldBe int

const (
	ShouldBeEqual ShouldBe = iota
	ShouldBeEqualOrLess
	ShouldBeEqualOrMore
)

func (s ShouldBe) String() string {
	switch s {
	case ShouldBeEqual:
		return "equal"
	case ShouldBeEqualOrLess:
		return "equal_or_less"
	case ShouldBeEqualOrMore:
		return "equal_or_more"
	default:
		return "equal"
	}
}

// ReconcileTo represents how to reconcile values
type ReconcileTo int

const (
	ReconcileToMin ReconcileTo = iota
	ReconcileToMax
	ReconcileToMean
	ReconcileToLatest
)

func (r ReconcileTo) String() string {
	switch r {
	case ReconcileToMin:
		return "min"
	case ReconcileToMax:
		return "max"
	case ReconcileToMean:
		return "mean"
	case ReconcileToLatest:
		return "latest"
	default:
		return "min"
	}
}

// Reconcile represents validation/correction rules
type Reconcile struct {
	ShouldBe    ShouldBe
	ReconcileTo ReconcileTo
}

// RemainderType represents the type of remainder calculation
type RemainderType int

const (
	RemainderParentState RemainderType = iota
	RemainderChildState
)

func (r RemainderType) String() string {
	switch r {
	case RemainderParentState:
		return "remaining_parent_state"
	case RemainderChildState:
		return "remaining_child_state"
	default:
		return "remaining_parent_state"
	}
}

// RemainderStrategy defines a calculated remainder entity
type RemainderStrategy struct {
	Key         string
	Label       string
	Type        RemainderType
	ChildrenSum *Reconcile // Optional
	ParentsSum  *Reconcile // Optional
}

// Group represents a group of sensors in a section
type Group struct {
	Name     string
	Section  Section
	Sensors  []Sensor
	Other    *RemainderStrategy // Optional remainder entity
	Children []string           // Child group names
}

// Config holds the complete sankey configuration
type Config struct {
	Sensors []SensorTemplate
	Groups  []Group
}
