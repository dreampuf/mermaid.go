// Package fixtures holds test inputs shared by the packages of mermaid.go.
package fixtures

// Diagrams are the families the chrome backend is tested against, so the
// merman backends are held to the same diagrams rather than to a merman-shaped
// subset.
var Diagrams = map[string]string{
	"Flowchart": `graph TD;
    A-->B;
    A-->C;
    B-->D;
    C-->D;`,
	"Sequence": `sequenceDiagram
    participant Alice
    participant Bob
    Alice->>John: Hello John, how are you?
    loop Healthcheck
    John->>John: Fight against hypochondria
    end
    Note right of John: Rational thoughts <br/>prevail!
    John-->>Alice: Great!`,
	"Gantt": `gantt
dateFormat  YYYY-MM-DD
title Adding GANTT diagram to mermaid
section A section
Completed task            :done,    des1, 2014-01-06,2014-01-08
Active task               :active,  des2, 2014-01-09, 3d`,
	"Class": `classDiagram
Class01 <|-- AveryLongClass : Cool
Class03 *-- Class04
Class07 : equals()
Class01 : size()`,
	"GitGraph": `gitGraph
       commit
       branch develop
       commit
       checkout main
       merge develop`,
	"Entity": `erDiagram
    CUSTOMER ||--o{ ORDER : places
    ORDER ||--|{ LINE-ITEM : contains`,
	"Journey": `journey
    title My working day
    section Go to work
      Make tea: 5: Me
      Do work: 1: Me, Cat`,
	"State": `stateDiagram-v2
    [*] --> Still
    Still --> Moving
    Moving --> [*]`,
	"Pie": `pie title Pets
    "Dogs" : 386
    "Cats" : 85`,
	"QuotedLabels": `graph TD;
    A-->B['name'];
    A-->C["pic"];`,
	"MarkdownLabel": "graph TD;\n\tA-->B[\"`Hello World`\"];\n\tB-->C;",
}
