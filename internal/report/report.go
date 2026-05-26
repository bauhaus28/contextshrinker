package report

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"contextshrinker/internal/db"
)

type HealthAudit struct {
	GeneratedAt time.Time
	GodObjects  []db.GodObjectResult
	BlackHoles  []db.BlackHoleResult
	Cycles      []db.CycleResult
	DeadCode    []db.DeadCodeResult
}

func RunAudit(database *db.Database) (*HealthAudit, error) {
	godObjects, err := database.GetGodObjects()
	if err != nil {
		return nil, fmt.Errorf("failed to get God Objects: %w", err)
	}

	blackHoles, err := database.GetBlackHoles()
	if err != nil {
		return nil, fmt.Errorf("failed to get inbound dependency functions: %w", err)
	}

	cycles, err := database.GetCycles()
	if err != nil {
		return nil, fmt.Errorf("failed to run cyclic dependency detection: %w", err)
	}

	deadCode, err := database.GetDeadCode()
	if err != nil {
		return nil, fmt.Errorf("failed to run dead code detection: %w", err)
	}

	return &HealthAudit{
		GeneratedAt: time.Now(),
		GodObjects:  godObjects,
		BlackHoles:  blackHoles,
		Cycles:      cycles,
		DeadCode:    deadCode,
	}, nil
}

func (audit *HealthAudit) RenderMarkdown(workspaceRoot string) string {
	var sb strings.Builder
	sb.WriteString("# ContextShrinker Architecture Health Report\n\n")
	sb.WriteString(fmt.Sprintf("Generated on: %s\n", audit.GeneratedAt.Format(time.RFC1123)))
	sb.WriteString(fmt.Sprintf("Target Workspace: `%s`\n\n", workspaceRoot))
	sb.WriteString("---\n\n")

	// 1. God Objects
	sb.WriteString("## 1. God Objects (High Outbound Coupling)\n")
	sb.WriteString("Identifies classes/structs containing the highest count of fields, functions, or methods, indicating poor separation of concerns.\n\n")
	if len(audit.GodObjects) == 0 {
		sb.WriteString("*No classes/structs with outbound containment found.*\n\n")
	} else {
		sb.WriteString("| Class/Struct | File | Outbound Containment Count |\n")
		sb.WriteString("|---|---|---|\n")
		for _, obj := range audit.GodObjects {
			relPath, _ := filepath.Rel(workspaceRoot, obj.FilePath)
			sb.WriteString(fmt.Sprintf("| `%s` | `%s` | %d |\n", obj.ClassName, relPath, obj.OutboundComplexity))
		}
		sb.WriteString("\n")
	}

	// 2. Black Holes
	sb.WriteString("## 2. Inbound Coupling hotspots (Black Holes)\n")
	sb.WriteString("Identifies functions called heavily from elsewhere in the codebase, highlighting critical shared dependencies.\n\n")
	if len(audit.BlackHoles) == 0 {
		sb.WriteString("*No function calls found.*\n\n")
	} else {
		sb.WriteString("| Function | File | Inbound Call Count |\n")
		sb.WriteString("|---|---|---|\n")
		for _, bh := range audit.BlackHoles {
			relPath, _ := filepath.Rel(workspaceRoot, bh.FilePath)
			sb.WriteString(fmt.Sprintf("| `%s` | `%s` | %d |\n", bh.FunctionName, relPath, bh.InboundDependencies))
		}
		sb.WriteString("\n")
	}

	// 3. Cycles
	sb.WriteString("## 3. Cyclic Import Dependencies\n")
	sb.WriteString("Identifies circular import paths (up to depth 5) that violate clean dependency hierarchy rules.\n\n")
	if len(audit.Cycles) == 0 {
		sb.WriteString("✅ **Clean!** *No cyclic import dependencies detected.*\n\n")
	} else {
		sb.WriteString("⚠️ **Warning: Cyclic imports detected!**\n\n")
		sb.WriteString("| Cycle Path File |\n")
		sb.WriteString("|---|\n")
		for _, cy := range audit.Cycles {
			relPath, _ := filepath.Rel(workspaceRoot, cy.FilePath)
			sb.WriteString(fmt.Sprintf("| `%s` |\n", relPath))
		}
		sb.WriteString("\n")
	}

	// 4. Dead Code
	sb.WriteString("## 4. Unused Private/Unexported Functions\n")
	sb.WriteString("Identifies unexported functions that have zero inbound callers.\n\n")
	if len(audit.DeadCode) == 0 {
		sb.WriteString("✅ *No dead unexported functions found.*\n\n")
	} else {
		sb.WriteString("| Unused Function | File |\n")
		sb.WriteString("|---|---|\n")
		for _, dc := range audit.DeadCode {
			relPath, _ := filepath.Rel(workspaceRoot, dc.FilePath)
			sb.WriteString(fmt.Sprintf("| `%s` | `%s` |\n", dc.FunctionName, relPath))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}
