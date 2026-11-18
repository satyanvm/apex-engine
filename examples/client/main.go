// Client compiles a sentence, shows the plan, and runs it.
//
//	APEX_URL=http://localhost:8700 APEX_API_KEY=... go run ./examples/client "List the late orders"
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/satyanvm/apex-engine/client"
)

func main() {
	ctx := context.Background()
	c := client.New(os.Getenv("APEX_URL"), os.Getenv("APEX_API_KEY"))

	cats, err := c.ListCatalogs(ctx)
	if err != nil {
		log.Fatal(err)
	}
	ids := make([]string, 0, len(cats))
	for _, cat := range cats {
		ids = append(ids, cat.ID)
	}

	res, err := c.Compile(ctx, client.CompileRequest{Prompt: strings.Join(os.Args[1:], " "), CatalogIDs: ids})
	if err != nil {
		log.Fatal(err) // there is no fallback graph: an error means no plan
	}
	for _, n := range res.Plan.Nodes {
		fmt.Printf("%-24s %s\n", n.Key, n.Type)
	}

	run, err := c.StartRun(ctx, client.RunRequest{DAG: res.Plan.DAG(), CatalogIDs: ids, Name: res.Plan.Name})
	if err != nil {
		log.Fatal(err)
	}
	run, err = c.WaitRun(ctx, run.ID, client.WaitOptions{StopAtApproval: true})
	if err != nil {
		log.Fatal(err)
	}
	for _, step := range run.Waiting() {
		// Your application decides here: ask a person, apply a policy, and so on.
		if err := c.Decide(ctx, run.ID, step.Key, true, "approved by the host application"); err != nil {
			log.Fatal(err)
		}
	}
	run, err = c.WaitRun(ctx, run.ID, client.WaitOptions{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(run.ID, run.Status)
}
