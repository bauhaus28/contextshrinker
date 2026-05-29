package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bauhaus28/contextshrinker/internal/db"
)

type AIMetrics struct {
	SCR float64 // Structural Clone Ratio
	DCR float64 // Dead Code Ratio
	BVI int     // Boundary Violations Index
	AOI float64 // Abstraction Overkill Index

	TotalFunctions  int64
	ClonedFunctions int64
	DeadFunctions   int64
	TotalClasses    int64
	InterfaceCount  int64
	ConcreteCount   int64
	ViolationsCount int
}

func CalculateAIMetrics(database *db.Database, boundaryRules []db.BoundaryRule) (*AIMetrics, error) {
	funcCount, classCount, interfaceCount, concreteCount, err := database.GetStats()
	if err != nil {
		return nil, fmt.Errorf("failed to get database stats: %w", err)
	}

	clones, err := database.GetClones()
	if err != nil {
		return nil, fmt.Errorf("failed to get clones: %w", err)
	}

	var clonedFuncsCount int64
	for _, group := range clones {
		clonedFuncsCount += int64(len(group.Functions))
	}

	deadCode, err := database.GetDeadCode()
	if err != nil {
		return nil, fmt.Errorf("failed to get dead code: %w", err)
	}
	deadFuncsCount := int64(len(deadCode))

	violations, err := database.GetBoundaryViolations(boundaryRules)
	if err != nil {
		return nil, fmt.Errorf("failed to get boundary violations: %w", err)
	}
	violationsCount := len(violations)

	scr := 0.0
	if funcCount > 0 {
		scr = float64(clonedFuncsCount) / float64(funcCount)
	}

	dcr := 0.0
	if funcCount > 0 {
		dcr = float64(deadFuncsCount) / float64(funcCount)
	}

	aoi := 0.0
	if concreteCount > 0 {
		aoi = float64(interfaceCount) / float64(concreteCount)
	}

	return &AIMetrics{
		SCR:             scr,
		DCR:             dcr,
		BVI:             violationsCount,
		AOI:             aoi,
		TotalFunctions:  funcCount,
		ClonedFunctions: clonedFuncsCount,
		DeadFunctions:   deadFuncsCount,
		TotalClasses:    classCount,
		InterfaceCount:  interfaceCount,
		ConcreteCount:   concreteCount,
		ViolationsCount: violationsCount,
	}, nil
}

func LoadBoundaryRules(workspaceRoot string) ([]db.BoundaryRule, error) {
	rulesPath := filepath.Join(workspaceRoot, ".contextshrinker", "boundaries.json")
	if _, err := os.Stat(rulesPath); os.IsNotExist(err) {
		return nil, nil
	}

	data, err := os.ReadFile(rulesPath)
	if err != nil {
		return nil, err
	}

	var rules []db.BoundaryRule
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, fmt.Errorf("failed to parse boundary rules json: %w", err)
	}

	return rules, nil
}
