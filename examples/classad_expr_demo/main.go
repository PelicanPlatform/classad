package main

import (
	"fmt"
	"log"

	"github.com/PelicanPlatform/classad/classad"
)

// Example demonstrating *classad.ClassAd and *classad.Expr fields in structs

func exampleClassAdFields() {
	fmt.Println("\n=== Example: *classad.ClassAd Fields ===")

	type ServiceConfig struct {
		Name     string           `classad:"Name"`
		Enabled  bool             `classad:"Enabled"`
		Database *classad.ClassAd `classad:"Database"`
		Cache    *classad.ClassAd `classad:"Cache"`
	}

	// Build nested ClassAds
	dbConfig := classad.New()
	dbConfig.InsertAttrString("host", "db.example.com")
	dbConfig.InsertAttr("port", 5432)
	dbConfig.InsertAttrString("name", "myapp")

	cacheConfig := classad.New()
	cacheConfig.InsertAttrString("type", "redis")
	cacheConfig.InsertAttrString("host", "cache.example.com")
	cacheConfig.InsertAttr("port", 6379)

	config := ServiceConfig{
		Name:     "myservice",
		Enabled:  true,
		Database: dbConfig,
		Cache:    cacheConfig,
	}

	// Marshal to ClassAd format
	classadStr, err := classad.Marshal(config)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Marshaled:")
	fmt.Println(classadStr)

	// Unmarshal back
	var restored ServiceConfig
	err = classad.Unmarshal(classadStr, &restored)
	if err != nil {
		log.Fatal(err)
	}

	// Access nested ClassAd values
	dbHost, _ := restored.Database.EvaluateAttrString("host")
	dbPort, _ := restored.Database.EvaluateAttrInt("port")
	cacheType, _ := restored.Cache.EvaluateAttrString("type")

	fmt.Printf("\nRestored values:\n")
	fmt.Printf("  Name: %s\n", restored.Name)
	fmt.Printf("  Database host: %s:%d\n", dbHost, dbPort)
	fmt.Printf("  Cache type: %s\n", cacheType)
}

func exampleExprFields() {
	fmt.Println("\n=== Example: *classad.Expr Fields ===")

	type JobTemplate struct {
		Name           string        `classad:"Name"`
		BaseCPUs       int           `classad:"BaseCPUs"`
		BaseMemory     int           `classad:"BaseMemory"`
		ComputedCPUs   *classad.Expr `classad:"ComputedCPUs"`
		ComputedMemory *classad.Expr `classad:"ComputedMemory"`
	}

	// Create expressions that will be evaluated later
	cpuExpr, _ := classad.ParseExpr("BaseCPUs * ScaleFactor")
	memExpr, _ := classad.ParseExpr("BaseMemory * ScaleFactor")

	template := JobTemplate{
		Name:           "batch-job",
		BaseCPUs:       2,
		BaseMemory:     4096,
		ComputedCPUs:   cpuExpr,
		ComputedMemory: memExpr,
	}

	// Marshal - expressions are preserved (not evaluated)
	classadStr, err := classad.Marshal(template)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Marshaled template:")
	fmt.Println(classadStr)

	// Unmarshal back
	var restored JobTemplate
	err = classad.Unmarshal(classadStr, &restored)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("\nExpression formulas (unevaluated):\n")
	fmt.Printf("  ComputedCPUs: %s\n", restored.ComputedCPUs.String())
	fmt.Printf("  ComputedMemory: %s\n", restored.ComputedMemory.String())

	// Evaluate with different scale factors
	// Note: Need to provide both BaseCPUs/BaseMemory and ScaleFactor
	fmt.Println("\nEvaluating with ScaleFactor=1:")
	context1 := classad.New()
	context1.InsertAttr("BaseCPUs", int64(restored.BaseCPUs))
	context1.InsertAttr("BaseMemory", int64(restored.BaseMemory))
	context1.InsertAttr("ScaleFactor", 1)
	cpus1 := restored.ComputedCPUs.Eval(context1)
	mem1 := restored.ComputedMemory.Eval(context1)
	cpusVal1, _ := cpus1.IntValue()
	memVal1, _ := mem1.IntValue()
	fmt.Printf("  CPUs: %d, Memory: %d MB\n", cpusVal1, memVal1)

	fmt.Println("\nEvaluating with ScaleFactor=4:")
	context2 := classad.New()
	context2.InsertAttr("BaseCPUs", int64(restored.BaseCPUs))
	context2.InsertAttr("BaseMemory", int64(restored.BaseMemory))
	context2.InsertAttr("ScaleFactor", 4)
	cpus2 := restored.ComputedCPUs.Eval(context2)
	mem2 := restored.ComputedMemory.Eval(context2)
	cpusVal2, _ := cpus2.IntValue()
	memVal2, _ := mem2.IntValue()
	fmt.Printf("  CPUs: %d, Memory: %d MB\n", cpusVal2, memVal2)
}

func exampleNilFields() {
	fmt.Println("\n=== Example: Nil *ClassAd and *Expr Fields ===")

	type OptionalConfig struct {
		Name     string           `classad:"Name"`
		Required int              `classad:"Required"`
		Database *classad.ClassAd `classad:"Database,omitempty"`
		Formula  *classad.Expr    `classad:"Formula,omitempty"`
	}

	// Create with nil optional fields
	config := OptionalConfig{
		Name:     "minimal",
		Required: 42,
		Database: nil,
		Formula:  nil,
	}

	classadStr, err := classad.Marshal(config)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Marshaled with nil fields:")
	fmt.Println(classadStr)

	// Unmarshal and check
	var restored OptionalConfig
	err = classad.Unmarshal(classadStr, &restored)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("\nRestored nil checks:\n")
	fmt.Printf("  Database is nil: %v\n", restored.Database == nil)
	fmt.Printf("  Formula is nil: %v\n", restored.Formula == nil)
}

func main() {
	fmt.Println("ClassAd and Expr Fields Examples")
	fmt.Println("=================================")

	exampleClassAdFields()
	exampleExprFields()
	exampleNilFields()
}
