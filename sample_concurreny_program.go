package main

import (
	"sync"

	"github.com/fatih/color"
)

func test1(out chan<- int, wg *sync.WaitGroup, workerId int, num ...int) {
	go func() {
		defer wg.Done()
		for _, n := range num {
			color.Red("worker %d is sending value %d", workerId, n)
			out <- n * n
			color.Blue("worker %d completed processing value %d", workerId, n)
		}
	}()
}

func sample() {
	wg := &sync.WaitGroup{}

	temp := make(chan int)
	test1(temp, wg, 1, 2, 10, 7, 8, 11, 239, 34, 234, 53, 23,535,235,98, 03)
	//test1(temp, wg, 2, 25, 4, 17, 12)

	wg.Add(1)

	go func() {
		for v := range temp {
			result := v
			color.Green("process by workerId 1 obtained value: %d", result)
		}
		close(temp)
	}()

	go func() {
		for v := range temp {
			result := v
			color.Yellow("process by workerId 2 obtained value: %d", result)
		}
		close(temp)
	}()

	wg.Wait()

}
